# hcloud-console-tty

Open a Hetzner Cloud server's console as **text in your terminal**.

Hetzner's web console is a noVNC session — pixels, not characters. This
tool speaks RFB over the same wss URL, but instead of rendering pixels it
**decodes the framebuffer back to characters** by hashing each 8×16 cell
against a known PSF font. The result is real, selectable text in your
terminal, with full keyboard input forwarded back as RFB key events.

## Requirements

- Go 1.21+ (uses `go:embed` and `coder/websocket`)
- The [`hcloud` CLI](https://github.com/hetznercloud/cli), authenticated
- A terminal with 24-bit colour and at least 80×25

## Build

```bash
go build -o ~/bin/hcloud-console-tty .
```

The font, PSF parser, RFB protocol, and ANSI renderer are all in this
directory; the resulting binary has no runtime dependencies.

## Usage

```bash
hcloud-console-tty <server-id-or-name>
```

Press **Ctrl+]** to disconnect and return to your local shell. The hint is
also set as the terminal title for the duration of the session. Ctrl+C and
all other control characters are forwarded to the remote shell.

### Flags

| Flag | Purpose |
| --- | --- |
| `-from-stdin` | Read hcloud output from stdin instead of invoking it |
| `-ws URL -pw PW` | Skip hcloud and connect directly with explicit credentials |
| `-once` | Render a single frame and exit (handy for scripting) |
| `-send STRING` | Scripted-input mode: send keystrokes and capture the result. Escapes: `\n` Enter, `\t` Tab, `\b` Backspace, `\e` Esc, `\^x` Ctrl+x, `\U \D \L \R` arrows, `\H \E` Home/End. |
| `-send-delay D` | Delay between scripted keystrokes (default 80ms) |
| `-settle D` | Wait this long after the last keystroke for the framebuffer to update (default 1.5s) |
| `-debug FILE` | Append per-step protocol/decoder traces to FILE |
| `-dump-fb FILE` | Save a PPM of the framebuffer (with `-once` or `-send`) |

### Verified keyboard handling

The following all work end-to-end against a Linux text-mode console:

- All ASCII printables, including capital letters and shifted symbols
  (`!@#$%^&*()_+{}|:<>?~"`). Note: QEMU's VNC server treats keysyms as
  US-layout physical keys and does not auto-shift, so we synthesise a
  Shift_L press/release around the base key for the affected codepoints.
- Backspace, Tab (with completion), Enter, Esc.
- Arrow keys for shell history and editor navigation.
- Function keys F1–F12, Home/End/PageUp/PageDown/Insert/Delete.
- Ctrl+letter combinations (Ctrl+C, Ctrl+D, Ctrl+L, Ctrl+U, Ctrl+R, …).
- vi: insert mode, normal-mode commands, **`:` line mode** (`:wq`,
  `:%s/foo/bar/g`, …), search (`/`, `?`).

## How it works

1. **Connect.** `coder/websocket` dials the wss URL with the `binary`
   subprotocol; the resulting `net.Conn` is fed straight into a normal RFB
   client.
2. **Handshake.** RFB 3.8 version exchange, security negotiation. Hetzner
   bakes auth into the URL and offers `None` at the RFB layer; VNC password
   auth (DES challenge-response) is implemented for compatibility with
   other endpoints.
3. **Force a known pixel format.** We ask the server for 32-bit BGRX little
   endian so framebuffer bytes are easy to read.
4. **Receive a framebuffer.** Only the `Raw` encoding is requested — small
   amount of code, supported by every server.
5. **Detect the cell grid.** Try common sizes (8×16, 9×16, 8×8) and pick
   the candidate with the highest glyph match rate.
6. **Decode each cell.** Find the two dominant colours (background and
   foreground), build a 16-byte 1-bit bitmap, look it up in the embedded
   PSF font's glyph→codepoint table. Exact match first; on miss, fall back
   to nearest-neighbour with vertical baseline shifts ±3 (handles fonts
   like X.Org "Fixed" that sit lower in the cell).
7. **Render.** Diff against the previous frame and emit ANSI cursor moves +
   24-bit colour escapes for the cells that changed.
8. **Forward keys.** Stdin is put in raw mode; bytes are decoded into X11
   keysyms (with Ctrl/Alt modifier press/release pairs around the keysym)
   and sent as RFB `KeyEvent` messages. ANSI escape sequences from the
   terminal (arrows, F-keys, Home/End/PageUp etc.) are recognised and
   translated.

## Limitations

- **Text mode only.** When the VM switches to a graphical mode (X server,
  framebuffer console with a custom font), match rate drops and the tool
  prints a notice instead of garbage. Open the browser console for those
  cases.
- **One font.** The embedded font is Debian's `Uni2-Fixed16` (8×16). The
  hamming-distance fallback handles small variations, but very different
  fonts (e.g. UEFI splash text in unusual fonts) won't decode.
- **No mouse, no clipboard.** Forwarding selection/paste through RFB is
  out of scope; copy text directly from your terminal's selection buffer.

## Font attribution

The embedded `console_8x16.psf` is `Uni2-Fixed16.psf` from Debian's
`console-setup` package, originally built from the X.Org "Fixed" 8×16 BDF
(MIT/X11 licensed).
