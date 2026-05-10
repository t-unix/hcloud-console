package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
)

// Mode selector
var flagTTY = flag.Bool("tty", false, "render the console in this terminal instead of opening a browser")

// VM picker
var flagSelect = flag.Bool("select", false, "interactively pick a server from `hcloud server list` (implicit when no server arg is given)")

// Shared flags
var (
	flagWS        = flag.String("ws", "", "explicit wss URL (use with -pw)")
	flagPW        = flag.String("pw", "", "explicit VNC password (use with -ws)")
	flagFromStdin = flag.Bool("from-stdin", false, "read hcloud output from stdin instead of running it")
	flagDebug     = flag.String("debug", "", "write a debug log to this file")
)

// Browser-mode flags
var (
	flagPrintOnly = flag.Bool("print-only", false, "(browser) print the noVNC URL and exit")
	flagNoOpen    = flag.Bool("no-open", false, "(browser) start the local server but don't launch a browser")
)

// TTY-mode flags
var (
	flagOnce      = flag.Bool("once", false, "(tty) connect, render one frame, exit")
	flagDumpFB    = flag.String("dump-fb", "", "(tty) write a PPM of the framebuffer (with -once or -send)")
	flagNoFonts   = flag.Bool("no-embedded-fonts", false, "(tty, debug) skip embedded fonts and rely on bootstrap discovery")
	flagSend      = flag.String("send", "", "(tty) scripted-input mode: send these keystrokes and exit. "+
		"Escapes: \\n=Enter \\t=Tab \\b=Backspace \\e=Esc \\^x=Ctrl+x \\U \\D \\L \\R = arrows.")
	flagSendDelay = flag.Duration("send-delay", 80_000_000, "(tty) delay between scripted keystrokes")
	flagSettleMS  = flag.Duration("settle", 1_500_000_000, "(tty) settle time after scripted input")
)

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr,
			"usage: hcloud-console [flags] <server-id-or-name>\n\n"+
				"Default: opens the noVNC web client in your browser.\n"+
				"With -tty: decodes the framebuffer and renders the console as text\n"+
				"in this terminal (Ctrl+] to disconnect).\n\n"+
				"Flags:\n")
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

	if *flagTTY {
		err = runTTY(wsURL, password)
	} else {
		err = runBrowser(wsURL, password)
	}
	if err != nil {
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
	var server string
	switch {
	case *flagSelect:
		var err error
		server, err = selectServer()
		if err != nil {
			return "", "", err
		}
	case flag.NArg() >= 1:
		server = flag.Arg(0)
	default:
		// No server argument and nothing else specified — drop into
		// the interactive picker.
		var err error
		server, err = selectServer()
		if err != nil {
			return "", "", err
		}
	}
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

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "hcloud-console:", err)
	os.Exit(1)
}

var debugLog io.Writer

func debug(format string, args ...any) {
	if debugLog == nil {
		return
	}
	fmt.Fprintf(debugLog, format+"\n", args...)
}
