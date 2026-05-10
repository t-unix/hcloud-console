package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
)

// noVNC v1.7.0 client, embedded so the binary is self-contained.
//
//go:embed all:web
var noVNCFS embed.FS

// runBrowser starts a tiny HTTP server on a random 127.0.0.1 port serving
// the embedded noVNC client and opens vnc.html in the user's browser with
// auto-connect parameters pointing at the wss URL Hetzner returned.
func runBrowser(wsURL, password string) error {
	host, port, path, err := splitWSForBrowser(wsURL)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	localPort := ln.Addr().(*net.TCPAddr).Port

	sub, err := fs.Sub(noVNCFS, "web")
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler: silentLogger{http.FileServer(http.FS(sub))},
	}
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(ln) }()

	params := url.Values{}
	params.Set("autoconnect", "1")
	params.Set("encrypt", "1")
	params.Set("host", host)
	params.Set("port", strconv.Itoa(port))
	params.Set("path", path)
	params.Set("password", password)
	params.Set("resize", "scale")
	params.Set("reconnect", "1")
	consoleURL := fmt.Sprintf("http://127.0.0.1:%d/vnc.html?%s",
		localPort, params.Encode())

	if *flagPrintOnly {
		fmt.Println(consoleURL)
		_ = srv.Close()
		return nil
	}

	fmt.Fprintf(os.Stderr, "noVNC URL: %s\n", consoleURL)
	if !*flagNoOpen {
		if err := openBrowser(consoleURL); err != nil {
			fmt.Fprintf(os.Stderr,
				"could not auto-open browser (%v) — copy the URL above\n", err)
		}
	}
	fmt.Fprintln(os.Stderr, "local server running. Press Ctrl+C to stop.")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\nstopping.")
	_ = srv.Close()
	<-srvDone
	return nil
}

// splitWSForBrowser pulls host, port and the rebuilt path-with-query out
// of a wss:// URL so we can hand them to noVNC's vnc.html parameters.
// Hetzner's URL has authentication tokens in the query string; we keep
// them URL-encoded so the round-trip through vnc.html?path=… preserves
// them exactly.
func splitWSForBrowser(wsURL string) (host string, port int, path string, err error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", 0, "", fmt.Errorf("parse wsURL: %w", err)
	}
	host = u.Hostname()
	switch {
	case u.Port() != "":
		port, _ = strconv.Atoi(u.Port())
	case u.Scheme == "wss":
		port = 443
	default:
		port = 80
	}
	path = u.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return host, port, path, nil
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Start()
}

// silentLogger wraps an http.Handler to suppress the default request logs.
type silentLogger struct{ h http.Handler }

func (s silentLogger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.h.ServeHTTP(w, r)
}
