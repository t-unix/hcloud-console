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

	"golang.org/x/term"
)

var (
	flagWS        = flag.String("ws", "", "explicit wss URL (use with -pw)")
	flagPW        = flag.String("pw", "", "explicit VNC password (use with -ws)")
	flagFromStdin = flag.Bool("from-stdin", false, "read hcloud output from stdin")
	flagDebug     = flag.String("debug", "", "write debug log to this file")
	flagOnce      = flag.Bool("once", false, "connect, render one frame, exit (non-interactive)")
	flagDumpFB    = flag.String("dump-fb", "", "write a PPM of the captured framebuffer (for debugging)")
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

	if *flagOnce {
		return runOnce(rfb)
	}

	checkTerminalSize(rfb)

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)
	defer fmt.Fprint(os.Stdout, ansiShowCursor+ansiReset)

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
	for r := 0; r < g.rows; r++ {
		for c := 0; c < g.cols; c++ {
			ch := g.cells[r][c].ch
			if ch == 0 {
				ch = ' '
			}
			fmt.Print(string(ch))
		}
		fmt.Println()
	}
	return nil
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
	// Most likely text grid is 80×25; warn if terminal is too small.
	if w < 80 || h < 25 {
		fmt.Fprintf(os.Stderr,
			"warning: terminal is %dx%d; console is typically 80x25\n",
			w, h)
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
