# hcloud-console

**Render any Hetzner Cloud VM's VGA console as real, selectable text in
your terminal.** Not OCR, not ASCII-art — deterministic 8×16 glyph
hashing against an embedded PSF font, with full keyboard input
forwarded back over RFB. Boot messages, login prompts, `vi`, `top`,
shell — all readable, copyable, scriptable, in 80×25 of crisp ANSI
text.

```text
Debian GNU/Linux 13 test2 tty1

test2 login: root
Password:
Linux test2 6.12.57+deb13-arm64 #1 SMP Debian 6.12.57-1 (2025-11-05) aarch64
…
root@test2:~# top
top - 14:32:11 up 1 day,  4:23,  1 user,  load average: 0.00, 0.00, 0.00
…
```

Hetzner's `hcloud server request-console <id>` only spits out a wss URL
and a VNC password. This binary does everything else: dials the
WebSocket, speaks RFB 3.8, decodes each cell of the framebuffer back to
the character that drew it, and renders the result with diff-based
24-bit-colour ANSI updates.

> **Not to be confused with**
> [hilbix/hcloud-console](https://github.com/hilbix/hcloud-console),
> which is an unrelated Python+MongoDB project that solves a similar
> problem with a self-hosted web service.

## What makes it different

| Approach              | What you get                                         | Tools that take it      |
| ---                   | ---                                                  | ---                     |
| **Glyph hashing**     | Real text in your terminal, pixel-perfect            | **this**                |
| OCR                   | Lossy text, slow, fails on transient frames          | dutchcoders/vncscan     |
| Pixel-block ASCII-art | Fuzzy approximation of the picture, no actual text   | HouzuoGuo/headmore, sidorares/ansi-vnc |
| Run noVNC in browser  | Full graphical fidelity but no terminal integration  | every cloud provider's web console |

The trick is recognising that a Linux text-mode framebuffer is just an
80×25 grid of 8×16 cells where each cell is one of ≤256 deterministic
glyph bitmaps. Hash the bitmap, look it up in the font, get the
codepoint. Done.

## Requirements

- Go 1.21+ to build (the binary is fully self-contained — RFB
  protocol, ANSI renderer, PSF fonts, and a copy of noVNC are all
  embedded; no runtime dependencies).
- The [`hcloud` CLI](https://github.com/hetznercloud/cli),
  authenticated.

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
hcloud-console <server-id-or-name>
```

That's it — your terminal becomes the VM's console. Press **Ctrl+]** to
disconnect. Resize the window and the framebuffer redraws cleanly.

```bash
hcloud-console --select   # pick a VM interactively (fzf over `hcloud server list`)
hcloud-console --once <id>  # one frame as plain text, exit (good for scripting)
```

### How it works

1. Dial the wss URL via `coder/websocket` with the `binary` subprotocol.
2. Speak RFB 3.8: version exchange, security negotiation (None or VNC
   DES challenge-response), force a 32-bit BGRX pixel format, request
   only the Raw encoding.
3. For each `FramebufferUpdate`, try every (cell-size × font)
   combination — `(8,16)`, `(9,16)`, `(8,8)` — and pick the one with
   the highest **exact** match rate on non-blank cells. Per cell:
   identify the two dominant colours, build a 16-byte 1-bit fg-mask,
   and look it up in the chosen font's glyph table.
4. Diff against the previous frame and emit ANSI cursor moves +
   24-bit-colour escapes for cells that changed. SIGWINCH triggers a
   full re-render so terminal resizes don't leave stale content.
5. Stdin in raw mode → X11 keysyms wrapped in Ctrl/Alt/Shift modifier
   press/release pairs. Shifted ASCII (`:` `?` `!` …) is mapped to
   base+Shift because QEMU's VNC server doesn't auto-shift. Arrow,
   F-keys, Home/End/PageUp etc. are translated from the terminal's
   ANSI escape sequences to RFB equivalents.

### Tested OS coverage

The decoder auto-selects the right font:

| OS                               | Cell size | Font picked        | Match rate |
| ---                              | ---       | ---                | ---        |
| Debian / Ubuntu (kernel fbcon)   | 8×16      | `uni2-fixed16`     | 100%       |
| OpenBSD (BIOS/VGA text mode)     | 9×16      | `seabios-vga-8x16` | 100%       |
| FreeBSD, NetBSD, BIOS-mode Linux | 9×16      | `seabios-vga-8x16` | (same path) |

To add an OS that uses a different font, drop a bitmap into `font.go`
and append to the `fonts` slice. The auto-selector picks it up.

### Bootstrap fallback for unknown fonts

If none of the embedded fonts is a good fit (match rate < 95%), the
decoder learns the font **from the framebuffer itself**. It scans for
~50 well-known literal anchor strings — `"checking"`, `"running"`,
`"Password:"`, `"OpenBSD"`, `"boot"`, `"root"`, etc. — and harvests
(bitmap → rune) pairs from successful matches.

Anchors with internal repeated letters (`"checking"` — `c` at
positions 0 and 3, `"running"` — `n` at 2/3/5) impose positional
constraints that random text almost never satisfies, so even a single
match is reliable enough to seed the font. Bitmaps are
cross-validated across multiple anchors and only accepted if there's a
clear winner.

In practice this gives ≈70% match rate on a fresh OS we have no font
for — readable boot messages and login prompt, with some
nearest-neighbour fuzziness for letters not seen in any anchor.

### Verified keyboard handling

- All ASCII printables, including capital letters and shifted symbols
  `!@#$%^&*()_+{}|:<>?~"`.
- Backspace, Tab (with completion), Enter, Esc.
- Arrow keys for shell history and editor navigation.
- Function keys F1–F12, Home/End/PageUp/PageDown/Insert/Delete.
- Ctrl+letter combinations (Ctrl+C, Ctrl+D, Ctrl+L, Ctrl+U, Ctrl+R …).
- `vi`: insert mode, normal-mode commands, **`:` line mode** (`:wq`,
  `:%s/foo/bar/g` …), search (`/`, `?`).

### Limitations

- Text mode only. When the VM switches to a graphical mode (X server,
  framebuffer console with a custom font, UEFI splash) the
  exact-match rate drops and the tool prints a notice — that's when
  the browser fallback below is useful.
- No mouse, no clipboard. Copy text directly from your terminal's
  selection buffer.

## Optional: noVNC fallback

If you need pixels — graphical installer, X session, custom-font
console — the same binary can launch the embedded noVNC client in
your default browser:

```bash
hcloud-console --novnc <server-id-or-name>     # alias: --graphical
hcloud-console --novnc --print-only <id>       # just print the URL
hcloud-console --novnc --no-open <id>          # serve, don't auto-launch
```

A tiny HTTP server on `127.0.0.1` serves the embedded noVNC v1.7.0
build; the browser opens `vnc.html` with auto-connect parameters
pointing at `web-console.hetzner.cloud`. The local server is just a
static file host — your browser still talks directly to Hetzner over
TLS.

## All flags

```text
hcloud-console [flags] <server-id-or-name>
```

**Selection / connection**

| Flag | Purpose |
| --- | --- |
| `--select` | Interactively pick a server with fuzzy search over `hcloud server list` |
| `--from-stdin` | Read hcloud output from stdin instead of running `hcloud` |
| `--ws URL --pw PW` | Skip `hcloud` and use explicit credentials |
| `--debug FILE` | Append protocol/decoder/SIGWINCH traces to FILE |

**Terminal mode (default)**

| Flag | Purpose |
| --- | --- |
| `--once` | Connect, render one frame as plain text, exit |
| `--send STRING` | Scripted-input mode: send keystrokes and capture the resulting frame. Escapes: `\n` Enter, `\t` Tab, `\b` Backspace, `\e` Esc, `\^x` Ctrl+x, `\U \D \L \R` arrows, `\H \E` Home/End. |
| `--send-delay D` | Delay between scripted keystrokes (default 80ms) |
| `--settle D` | Settle time after the last scripted keystroke (default 1.5s) |
| `--dump-fb FILE` | Write a PPM of the framebuffer (with `--once` or `--send`) |
| `--no-embedded-fonts` | Skip embedded fonts; force the bootstrap path (debug) |

**Browser mode (`--novnc` / `--graphical`)**

| Flag | Purpose |
| --- | --- |
| `--print-only` | Print the noVNC URL and exit |
| `--no-open` | Start the local server but don't launch a browser |

## Attributions

- noVNC v1.7.0 client (in `web/`) — © The noVNC authors, MPL-2.0.
- `console_8x16.psf` — `Uni2-Fixed16.psf` from Debian's `console-setup`
  package, originally built from the X.Org "Fixed" 8×16 BDF (MIT/X11).
- `vga_8x16.bin` — SeaBIOS/QEMU `vgafont16` (the font QEMU's emulated
  VGA card actually renders to the framebuffer); derived from
  public-domain DOS 8×16 fonts collected by Joseph Gil.
