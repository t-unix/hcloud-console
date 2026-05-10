package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/term"
)

// runTTY drives terminal-mode rendering: connect, decode framebuffers
// back to text, render with ANSI, forward keystrokes from stdin.
func runTTY(wsURL, password string) error {
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
	fmt.Fprintf(os.Stdout,
		"\x1b]0;hcloud-console • %s • Ctrl+] disconnects\x07",
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
	go func() { <-errCh }()

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
		fmt.Fprintln(os.Stderr,
			"low match rate — printing anyway (likely graphical mode or unknown font)")
	}
	printGrid(g)
	return nil
}

func printGrid(g *grid) {
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
//	\H  Home         \E  End
func parseScript(s string) ([]keyEvent, error) {
	var out []keyEvent
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, keyEvent{keysym: uint32(c)})
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

// runSend connects, waits for the first frame, sends a script of keystrokes,
// captures the resulting frame, and prints it as plain text.
func runSend(rfb *rfbConn, script string) error {
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

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- consumeFrames(rfb, stop) }()

	for _, ev := range keys {
		if err := sendKey(rfb, ev); err != nil {
			close(stop)
			<-done
			return err
		}
		time.Sleep(*flagSendDelay)
	}

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
		case 0:
			if _, err := rfb.readFramebufferUpdate(); err != nil {
				return err
			}
			g := decode(rfb)
			// Threshold 0.5 instead of 0.85 — anchor-bootstrapped
			// fonts typically score 60–80% on unknown systems and
			// the partial output is still readable. Truly graphical
			// frames score near zero because no glyph patterns exist.
			if g != nil && g.matchRate >= 0.5 {
				rend.draw(g)
				debug("frame: %dx%d cells %dx%d font=%s match=%.2f",
					g.cols, g.rows, g.cellW, g.cellH,
					fontName(g), g.matchRate)
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

func fontName(g *grid) string {
	if g == nil || g.font == nil {
		return "?"
	}
	return g.font.name
}

func checkTerminalSize(rfb *rfbConn) {
	if !term.IsTerminal(int(os.Stdout.Fd())) {
		return
	}
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return
	}
	cols := rfb.width / 8
	rows := rfb.height / 16
	if w < cols || h < rows {
		fmt.Fprintf(os.Stderr,
			"warning: terminal is %dx%d but the console grid is %dx%d — "+
				"content past the edges will be clipped/wrapped\n",
			w, h, cols, rows)
	}
}
