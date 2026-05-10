# hcloud-console

Open a Hetzner Cloud server's console without leaving the terminal —
either as a noVNC session in your browser, or rendered as actual text
right in your shell.

`hcloud server request-console <id>` only prints a WebSocket URL and a
VNC password. This single Go binary does the heavy lifting:

- **Browser mode** (default): serves the embedded
  [noVNC](https://github.com/novnc/noVNC) client over `127.0.0.1` and
  opens `vnc.html` in your default browser with auto-connect.
- **Terminal mode** (`--tty`): speaks RFB over the same wss URL, decodes
  each 8×16 framebuffer cell back to a character by hashing it against
  an embedded PSF font, and renders the console as ANSI text with full
  keyboard input. **Not OCR, not ASCII-art** — deterministic glyph
  matching, so the output is real selectable text.

The browser talks directly to `web-console.hetzner.cloud` over TLS in
both modes — the local server is just a static file host for the noVNC
JS, never a proxy.

> **Not to be confused with** [hilbix/hcloud-console](https://github.com/hilbix/hcloud-console),
> which is an unrelated Python+MongoDB project that solves a similar
> problem with a self-hosted web service.

## Requirements

- Go 1.21+ to build (the binary is fully self-contained: noVNC client,
  PSF font, RFB protocol, and ANSI renderer are all embedded).
- The [`hcloud` CLI](https://github.com/hetznercloud/cli), authenticated.

## Install

```bash
go install github.com/t-unix/hcloud-console@latest
```

Or build from source:

```bash
git clone https://github.com/t-unix/hcloud-console
cd hcloud-console
go build -o ~/bin/hcloud-console .
```

## Usage

```bash
# Browser (default)
hcloud-console <server-id-or-name>

# Terminal — Ctrl+] to disconnect
hcloud-console --tty <server-id-or-name>
```

### Common flags

| Flag | Purpose |
| --- | --- |
| `--tty` | Render the console as text in this terminal instead of opening a browser |
| `--from-stdin` | Read hcloud output from stdin instead of running `hcloud` |
| `--ws URL --pw PW` | Skip `hcloud` and use explicit credentials |
| `--debug FILE` | Append protocol/decoder traces to FILE |

### Browser-mode flags

| Flag | Purpose |
| --- | --- |
| `--print-only` | Print the noVNC URL and exit (no server, no browser) |
| `--no-open` | Start the local server but don't launch a browser |

### Terminal-mode flags

| Flag | Purpose |
| --- | --- |
| `--once` | Connect, render one frame as plain text, exit |
| `--send STRING` | Scripted-input mode: send keystrokes and capture the resulting frame. Escapes: `\n` Enter, `\t` Tab, `\b` Backspace, `\e` Esc, `\^x` Ctrl+x, `\U \D \L \R` arrows, `\H \E` Home/End. |
| `--send-delay D` | Delay between scripted keystrokes (default 80ms) |
| `--settle D` | Settle time after the last scripted keystroke (default 1.5s) |
| `--dump-fb FILE` | Write a PPM of the framebuffer (with `--once` or `--send`) |

## How the modes work

### Browser

The wss URL Hetzner returns has authentication tokens baked into its
query string. The wrapper preserves them by URL-encoding the entire path
(including the query) once, then passing it to noVNC's `vnc.html?path=…`
parameter. noVNC decodes it once and uses it verbatim when opening the
WebSocket, so the tokens reach Hetzner's backend exactly as issued.
Ctrl+C stops the local server.

### Terminal

1. Dial the wss URL via `coder/websocket` with the `binary` subprotocol.
2. Speak RFB 3.8: version exchange, security negotiation (None or VNC
   password DES challenge-response), force a 32-bit BGRX pixel format,
   request only Raw encoding.
3. For each `FramebufferUpdate`, detect the cell grid (8×16 / 9×16 /
   8×8), find the two dominant colours per cell, build a 16-byte 1-bit
   bitmap, and look it up in the embedded PSF font's glyph→codepoint
   table. Exact match first; nearest-neighbour with vertical baseline
   shifts ±3 as a tolerance fallback.
4. Diff against the previous frame and emit ANSI cursor moves + 24-bit
   colour escapes for cells that changed.
5. Stdin in raw mode → X11 keysyms wrapped in Ctrl/Alt/Shift modifier
   pairs as appropriate (QEMU's VNC doesn't auto-shift, so we do it for
   shifted ASCII like `:` `?` `!` etc.). ANSI escape sequences from the
   terminal (arrows, F-keys, Home/End/PageUp etc.) are translated to
   their RFB equivalents.

### Verified keyboard handling

- All ASCII printables, including capital letters and shifted symbols
  `!@#$%^&*()_+{}|:<>?~"`.
- Backspace, Tab (with completion), Enter, Esc.
- Arrow keys for shell history and editor navigation.
- Function keys F1–F12, Home/End/PageUp/PageDown/Insert/Delete.
- Ctrl+letter combinations (Ctrl+C, Ctrl+D, Ctrl+L, Ctrl+U, Ctrl+R, …).
- vi: insert mode, normal-mode commands, **`:` line mode** (`:wq`,
  `:%s/foo/bar/g`, …), search (`/`, `?`).

### Tested OS coverage

The decoder tries every (cell-size × font) combination and picks the one
with the highest exact-match rate on non-blank cells, so it auto-selects
the right font for whatever the VM is running:

| OS                                | Cell size | Font picked        | Match rate |
| --- | --- | --- | --- |
| Debian / Ubuntu (kernel fbcon)    | 8×16      | `uni2-fixed16`     | 100% |
| OpenBSD (BIOS/VGA text mode)      | 9×16      | `seabios-vga-8x16` | 100% |
| FreeBSD, NetBSD, BIOS-mode Linux  | 9×16      | `seabios-vga-8x16` | (same path) |

To add another OS that uses a different font, drop the bitmap into
`font.go` and append to the `fonts` slice — the auto-selector picks it
up automatically.

### Bootstrap fallback (unknown fonts)

If none of the embedded fonts is a good fit (match rate < 95%), the
decoder tries to **learn the font from the framebuffer itself** by
searching for well-known literal anchor strings — `"checking"`,
`"running"`, `"Password:"`, `"OpenBSD"`, `"boot"`, `"root"`, etc. Each
match contributes (bitmap → rune) pairs that are then cross-validated
across multiple anchors before being accepted into a runtime font.

Anchors with internal repeated letters (`"checking"` — 'c' at positions
0 and 3, `"running"` — 'n' at 2/3/5, etc.) impose positional constraints
that random text almost never satisfies, so a single match of one of
those is reliable enough to seed the font. Anchors that match multiple
times, anchors with multiple internal repeats, or bitmaps voted for by
more than one anchor are all treated as high-confidence; everything else
is dropped.

In practice this gives ≈70% match rate on a fresh OS we don't have a
font for — readable boot messages and login prompt, with some
nearest-neighbour fuzziness for letters not seen in any anchor. Add the
font properly when you encounter the system regularly.

### Limitations of terminal mode

- Text mode only. When the VM switches to a graphical mode (X server,
  framebuffer console with a custom font), exact-match rate drops and
  the tool prints a notice. Open the browser console for those.
- No mouse, no clipboard. Copy text directly from your terminal's
  selection buffer.

## Attributions

- noVNC v1.7.0 client (in `web/`) — © The noVNC authors, MPL-2.0.
- `console_8x16.psf` — `Uni2-Fixed16.psf` from Debian's `console-setup`
  package, originally built from the X.Org "Fixed" 8×16 BDF (MIT/X11
  licensed).
- `vga_8x16.bin` — SeaBIOS/QEMU `vgafont16` (the font QEMU's emulated
  VGA card actually renders to the framebuffer); originally derived
  from public-domain DOS 8×16 fonts collected by Joseph Gil.
