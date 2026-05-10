# hcloud-console

Open a Hetzner Cloud server's VNC console in your browser, in one command.

`hcloud server request-console <id>` only prints a WebSocket URL and a VNC
password — it doesn't actually open anything. This wrapper parses that output,
serves a local copy of [noVNC](https://github.com/novnc/noVNC), and launches
your browser pointed at the right session with `autoconnect=1`.

The browser talks directly to `web-console.hetzner.cloud` over TLS; no
websockify proxy is involved.

## Requirements

- Python 3.10+
- The [`hcloud` CLI](https://github.com/hetznercloud/cli), authenticated
- A local copy of noVNC's HTML/JS (no daemon, no install — just the files)

## Install

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

## Usage

```bash
hcloud-console <server-id-or-name>
```

The script:

1. Runs `hcloud server request-console <server>`.
2. Parses out the WebSocket URL and VNC password.
3. Starts a tiny HTTP server on a random `127.0.0.1` port serving the noVNC
   files.
4. Opens your default browser at `vnc.html?…&autoconnect=1`.

Ctrl+C in the terminal stops the local HTTP server.

### Flags

| Flag | Purpose |
| --- | --- |
| `--from-stdin` | Read hcloud output from stdin instead of invoking hcloud |
| `--print-only` | Print the noVNC URL and exit (no server, no browser) |
| `--novnc-dir PATH` | Override the noVNC directory |
| `-- …` | Anything after `--` is forwarded to `hcloud` |

## How the URL is built

The wss URL Hetzner returns carries authentication tokens in its query string.
The wrapper preserves them by URL-encoding the entire path (including the
query) once, then passing it to noVNC's `vnc.html?path=…` parameter. noVNC
decodes it once and uses it verbatim when opening the WebSocket, so the
tokens reach Hetzner's backend exactly as issued.
