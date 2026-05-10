package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/term"
)

var (
	flagWS        = flag.String("ws", "", "explicit wss URL (use with -pw)")
	flagPW        = flag.String("pw", "", "explicit VNC password (use with -ws)")
	flagFromStdin = flag.Bool("from-stdin", false, "read hcloud output from stdin")
	flagDebug     = flag.String("debug", "", "write debug log to this file")
	flagOnce      = flag.Bool("once", false, "connect, render one frame, exit (non-interactive)")
	flagDumpFB    = flag.String("dump-fb", "", "write a PPM of the captured framebuffer (for debugging)")
	flagSend      = flag.String("send", "", "scripted-input mode: send these keystrokes and exit. "+
		"Escapes: \\n=Enter \\t=Tab \\b=Backspace \\e=Esc \\\\=backslash. "+
		"Capture final frame; combine with -dump-fb to save the framebuffer.")
	flagSendDelay = flag.Duration("send-delay", 80_000_000, "delay between scripted keystrokes (default 80ms)")
	flagSettleMS  = flag.Duration("settle", 1_500_000_000, "after sending, wait this long for the framebuffer to settle (default 1.5s)")
)

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr,
			"usage: hcloud-console-tty [flags] <server-id-or-name>\n\n"+
				"Opens a Hetzner Cloud server's text-mode console in your terminal.\n"+
				"Press Ctrl+] to exit.\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	wsURL, password, err := obtainCredentials()
	if err != nil {
		fatal(err)
	}

	if *flagDebug != "" {
		f, err := os.Create(*flagDebug)
		if err != nil {
			fatal(err)
		}
		debugLog = f
		defer f.Close()
	}

	if err := run(wsURL, password); err != nil {
		fatal(err)
	}
}

func obtainCredentials() (string, string, error) {
	if *flagWS != "" && *flagPW != "" {
		return *flagWS, *flagPW, nil
	}
	if *flagFromStdin {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", "", fmt.Errorf("read stdin: %w", err)
		}
		return parseHcloudOutput(string(b))
	}
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	server := flag.Arg(0)
	fmt.Fprintf(os.Stderr, "Requesting console for %s...\n", server)
	out, err := exec.Command("hcloud", "server", "request-console", server).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("hcloud failed: %v\n%s", err, out)
	}
	return parseHcloudOutput(string(out))
}

var (
	reWS = regexp.MustCompile(`WebSocket URL:\s*(\S+)`)
	rePW = regexp.MustCompile(`VNC Password:\s*(\S+)`)
)

func parseHcloudOutput(s string) (string, string, error) {
	ws := reWS.FindStringSubmatch(s)
	pw := rePW.FindStringSubmatch(s)
	if ws == nil || pw == nil {
		return "", "", fmt.Errorf("could not parse hcloud output:\n%s", s)
	}
	return ws[1], pw[1], nil
}

func run(wsURL, password string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer cancel()

	rfb, err := dialRFB(ctx, wsURL, password)
	if err != nil {
		return err
	}
	defer rfb.Close()

	debug("connected: %dx%d %q", rfb.width, rfb.height, rfb.name)

	if *flagSend != "" {
		return runSend(rfb, *flagSend)
	}

	if *flagOnce {
		return runOnce(rfb)
	}

	checkTerminalSize(rfb)

	fmt.Fprintf(os.Stderr,
		"connected to %s — press \x1b[1mCtrl+]\x1b[0m to disconnect\n",
		rfb.name)
	// Set terminal title so the shortcut is always visible.
	fmt.Fprintf(os.Stdout,
		"\x1b]0;hcloud-console-tty • %s • Ctrl+] disconnects\x07",
		rfb.name)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)
	defer fmt.Fprint(os.Stdout, ansiShowCursor+ansiReset+"\x1b]0;\x07")

	rend := newRenderer(os.Stdout)
	rend.reset()

	if err := rfb.requestUpdate(false); err != nil {
		return err
	}

	errCh := make(chan error, 2)
	go func() { errCh <- readLoop(rfb, rend) }()
	go func() { errCh <- pumpKeys(rfb, os.Stdin) }()

	var firstErr error
	select {
	case firstErr = <-errCh:
	case <-ctx.Done():
		firstErr = ctx.Err()
	}
	rfb.Close()
	go func() { <-errCh }() // drain

	if firstErr == io.EOF || firstErr == nil {
		return nil
	}
	return firstErr
}

// runOnce performs handshake, requests a single frame, decodes, prints, exits.
// Useful for smoke-testing where stdin isn't a TTY.
func runOnce(rfb *rfbConn) error {
	if err := rfb.requestUpdate(false); err != nil {
		return err
	}
	for {
		t, err := rfb.readMessageType()
		if err != nil {
			return err
		}
		if t == 0 {
			if _, err := rfb.readFramebufferUpdate(); err != nil {
				return err
			}
			break
		}
		if err := rfb.drainOther(t); err != nil {
			return err
		}
	}

	if *flagDumpFB != "" {
		if err := dumpPPM(rfb, *flagDumpFB); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s (%dx%d)\n", *flagDumpFB, rfb.width, rfb.height)
	}

	g := decode(rfb)
	if g == nil {
		fmt.Fprintf(os.Stderr, "framebuffer %dx%d: no plausible cell size\n",
			rfb.width, rfb.height)
		return nil
	}
	fmt.Fprintf(os.Stderr,
		"framebuffer %dx%d, cells %dx%d (%dx%d), match rate %.1f%%\n",
		rfb.width, rfb.height, g.cellW, g.cellH, g.cols, g.rows,
		g.matchRate*100)

	if g.matchRate < 0.85 {
		fmt.Fprintln(os.Stderr, "low match rate — likely graphical mode")
		return nil
	}
	printGrid(g)
	return nil
}

func printGrid(g *grid) {
	// Trim trailing all-blank rows so output isn't padded with empty lines.
	last := -1
	for r := 0; r < g.rows; r++ {
		nonBlank := false
		for c := 0; c < g.cols; c++ {
			if ch := g.cells[r][c].ch; ch != 0 && ch != ' ' {
				nonBlank = true
				break
			}
		}
		if nonBlank {
			last = r
		}
	}
	for r := 0; r <= last; r++ {
		var line []rune
		for c := 0; c < g.cols; c++ {
			ch := g.cells[r][c].ch
			if ch == 0 {
				ch = ' '
			}
			line = append(line, ch)
		}
		// Trim trailing spaces on each line.
		i := len(line)
		for i > 0 && line[i-1] == ' ' {
			i--
		}
		fmt.Println(string(line[:i]))
	}
}

// parseScript turns a script string into the same keyEvent stream that
// readKey would produce from a real terminal. Escape sequences:
//
//	\n  Return       \t  Tab        \b  Backspace
//	\e  Escape       \\  literal backslash
//	\^x Ctrl+x       \U  Up         \D  Down       \L  Left      \R  Right
//	\H  Home         \E  End        \PgUp \PgDn   \F1..\F12
func parseScript(s string) ([]keyEvent, error) {
	var out []keyEvent
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, asciiToEvent(c))
			continue
		}
		i++
		if i >= len(s) {
			return nil, fmt.Errorf("trailing backslash in script")
		}
		switch s[i] {
		case 'n':
			out = append(out, keyEvent{keysym: ksReturn})
		case 't':
			out = append(out, keyEvent{keysym: ksTab})
		case 'b':
			out = append(out, keyEvent{keysym: ksBackspace})
		case 'e':
			out = append(out, keyEvent{keysym: ksEscape})
		case '\\':
			out = append(out, keyEvent{keysym: '\\'})
		case 'U':
			out = append(out, keyEvent{keysym: ksUp})
		case 'D':
			out = append(out, keyEvent{keysym: ksDown})
		case 'L':
			out = append(out, keyEvent{keysym: ksLeft})
		case 'R':
			out = append(out, keyEvent{keysym: ksRight})
		case 'H':
			out = append(out, keyEvent{keysym: ksHome})
		case 'E':
			out = append(out, keyEvent{keysym: ksEnd})
		case '^':
			i++
			if i >= len(s) {
				return nil, fmt.Errorf("\\^ at end of script")
			}
			letter := s[i]
			if letter >= 'A' && letter <= 'Z' {
				letter += 32
			}
			if letter < 'a' || letter > 'z' {
				return nil, fmt.Errorf("\\^%c: only letters supported", letter)
			}
			out = append(out, keyEvent{keysym: uint32(letter), ctrl: true})
		default:
			return nil, fmt.Errorf("unknown escape \\%c", s[i])
		}
	}
	return out, nil
}

// asciiToEvent mirrors readKey for printable ASCII: it just emits the byte
// as a keysym. The shifted-character mapping lives inside sendKey.
func asciiToEvent(b byte) keyEvent {
	return keyEvent{keysym: uint32(b)}
}

// runSend connects, waits for the first frame, sends a script of keystrokes
// (with one-second settle time at the end), captures the resulting frame,
// and prints it as plain text. Used for automated keyboard testing.
func runSend(rfb *rfbConn, script string) error {
	// Get one frame so we know the screen is ready.
	if err := rfb.requestUpdate(false); err != nil {
		return err
	}
	if err := drainOneFrame(rfb); err != nil {
		return err
	}

	keys, err := parseScript(script)
	if err != nil {
		return err
	}

	// Background goroutine to keep consuming framebuffer updates so the
	// server doesn't stall on us. We also need it so we have an up-to-date
	// pixel buffer at the end.
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- consumeFrames(rfb, stop)
	}()

	for _, ev := range keys {
		if err := sendKey(rfb, ev); err != nil {
			close(stop)
			<-done
			return err
		}
		time.Sleep(*flagSendDelay)
	}

	// Wait for the screen to settle, then take a snapshot.
	time.Sleep(*flagSettleMS)
	close(stop)
	<-done

	if *flagDumpFB != "" {
		if err := dumpPPM(rfb, *flagDumpFB); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *flagDumpFB)
	}

	g := decode(rfb)
	if g == nil || g.matchRate < 0.85 {
		fmt.Fprintln(os.Stderr, "framebuffer is not in text mode")
		return nil
	}
	printGrid(g)
	return nil
}

// drainOneFrame reads one FramebufferUpdate (skipping any Bell/Cut messages).
func drainOneFrame(rfb *rfbConn) error {
	for {
		t, err := rfb.readMessageType()
		if err != nil {
			return err
		}
		if t == 0 {
			_, err := rfb.readFramebufferUpdate()
			return err
		}
		if err := rfb.drainOther(t); err != nil {
			return err
		}
	}
}

// consumeFrames keeps requesting and reading incremental framebuffer
// updates until stop is closed. It runs in a goroutine so the main thread
// can dispatch keystrokes without blocking.
func consumeFrames(rfb *rfbConn, stop <-chan struct{}) error {
	if err := rfb.requestUpdate(true); err != nil {
		return err
	}
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		t, err := rfb.readMessageType()
		if err != nil {
			return err
		}
		if t == 0 {
			if _, err := rfb.readFramebufferUpdate(); err != nil {
				return err
			}
			if err := rfb.requestUpdate(true); err != nil {
				return err
			}
		} else {
			if err := rfb.drainOther(t); err != nil {
				return err
			}
		}
	}
}

func readLoop(rfb *rfbConn, rend *renderer) error {
	for {
		t, err := rfb.readMessageType()
		if err != nil {
			return err
		}
		switch t {
		case 0: // FramebufferUpdate
			if _, err := rfb.readFramebufferUpdate(); err != nil {
				return err
			}
			g := decode(rfb)
			if g != nil && g.matchRate >= 0.85 {
				rend.draw(g)
				debug("frame: %dx%d cells %dx%d match=%.2f",
					g.cols, g.rows, g.cellW, g.cellH, g.matchRate)
			} else {
				rend.drawNotice(rfb, g)
				if g != nil {
					debug("graphical frame: match=%.2f", g.matchRate)
				}
			}
			if err := rfb.requestUpdate(true); err != nil {
				return err
			}
		default:
			if err := rfb.drainOther(t); err != nil {
				return err
			}
		}
	}
}

func checkTerminalSize(rfb *rfbConn) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	// Estimate the cell grid (assume 8×16 cells, the common case).
	cols := rfb.width / 8
	rows := rfb.height / 16
	if w < cols || h < rows {
		fmt.Fprintf(os.Stderr,
			"warning: terminal is %dx%d but the console grid is %dx%d — "+
				"content past the edges will be clipped/wrapped\n",
			w, h, cols, rows)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "hcloud-console-tty:", err)
	os.Exit(1)
}

var debugLog io.Writer

func debug(format string, args ...any) {
	if debugLog == nil {
		return
	}
	fmt.Fprintf(debugLog, format+"\n", args...)
}
