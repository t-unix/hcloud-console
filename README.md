# hcloud-console

Two ways to open a Hetzner Cloud server's console without leaving the
terminal:

- **`hcloud-console`** — Python wrapper that launches a local
  [noVNC](https://github.com/novnc/noVNC) session in your browser. Same UX
  as the Hetzner Cloud Console, just one command.
- **`hcloud-console-tty`** ([`tty/`](tty/)) — Go tool that decodes the VNC
  framebuffer back into characters and renders the console *in your
  terminal* with full keyboard input. No browser involved.

Both wrappers parse the output of `hcloud server request-console <id>`,
which by itself only prints a WebSocket URL and a VNC password — the
heavy lifting (serving noVNC, or speaking RFB and translating
framebuffers) lives in this repo.

## hcloud-console (browser)

### Requirements

- Python 3.10+
- The [`hcloud` CLI](https://github.com/hetznercloud/cli), authenticated
- A local copy of noVNC's HTML/JS (no daemon, no install — just the files)

### Install

```bash
# 1. Drop noVNC somewhere on disk (one-time)
curl -sSL https://github.com/novnc/noVNC/archive/refs/tags/v1.7.0.tar.gz \
  | tar -xz -C ~/.local/share \
  && mv ~/.local/share/noVNC-1.7.0 ~/.local/share/novnc

# 2. Drop the wrapper on your PATH
curl -sSL https://raw.githubusercontent.com/t-unix/hcloud-console/main/hcloud-console \
  -o ~/bin/hcloud-console && chmod +x ~/bin/hcloud-console
```

If your noVNC lives elsewhere, point the script at it via
`HCLOUD_CONSOLE_NOVNC_DIR` or `--novnc-dir`.

### Usage

```bash
hcloud-console <server-id-or-name>
```

The script:

1. Runs `hcloud server request-console <server>`.
2. Parses out the WebSocket URL and VNC password.
3. Starts a tiny HTTP server on a random `127.0.0.1` port serving the noVNC
   files.
4. Opens your default browser at `vnc.html?…&autoconnect=1`.

Ctrl+C in the terminal stops the local HTTP server. The browser talks
directly to `web-console.hetzner.cloud` over TLS; no websockify proxy is
involved.

#### Flags

| Flag | Purpose |
| --- | --- |
| `--from-stdin` | Read hcloud output from stdin instead of invoking hcloud |
| `--print-only` | Print the noVNC URL and exit (no server, no browser) |
| `--novnc-dir PATH` | Override the noVNC directory |
| `-- …` | Anything after `--` is forwarded to `hcloud` |

#### How the URL is built

The wss URL Hetzner returns carries authentication tokens in its query string.
The wrapper preserves them by URL-encoding the entire path (including the
query) once, then passing it to noVNC's `vnc.html?path=…` parameter. noVNC
decodes it once and uses it verbatim when opening the WebSocket, so the
tokens reach Hetzner's backend exactly as issued.

## hcloud-console-tty (terminal)

Renders the console as actual text in your terminal — no browser, no GUI,
no OCR. The framebuffer is decoded back to characters by hashing each
8×16 cell against the same PSF font Linux has loaded.

See [`tty/README.md`](tty/README.md) for build instructions and details.

```bash
cd tty && go build -o ~/bin/hcloud-console-tty .
hcloud-console-tty <server-id-or-name>      # press Ctrl+] to exit
```
