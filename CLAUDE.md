# CLAUDE.md

Project-specific guidance for Claude Code working on this repo.

## What this is

Single Go binary called `hcloud-console`. Wraps `hcloud server
request-console <id>` (Hetzner Cloud CLI, prints a wss URL + VNC
password) and either:

- **Default**: speaks RFB-over-WebSocket itself, decodes the
  framebuffer back to characters via PSF glyph hashing, and renders
  the console as ANSI text in the user's terminal with full keyboard
  input forwarded as RFB `KeyEvent` messages. Ctrl+] to disconnect.
- **`--novnc` / `--graphical`**: serves the embedded noVNC client
  over `127.0.0.1` and opens `vnc.html` in the user's default browser
  with auto-connect parameters.

The browser/JS talks directly to `web-console.hetzner.cloud` over TLS
in both modes; the local HTTP server is just a static file host.

## File map

| File | Purpose |
| --- | --- |
| `main.go` | Entry, flags, dispatch (`runTTY` vs `runBrowser`), `hcloud` invocation, output parsing |
| `tty.go` | Terminal mode: dial RFB, raw mode, render/key goroutines, `runOnce`/`runSend` test modes |
| `browser.go` | Browser mode: HTTP server over embedded noVNC (`web/`), URL construction, `openBrowser` |
| `rfb.go` | RFB 3.8 protocol over WS (`coder/websocket` → `net.Conn`), `FramebufferUpdate` parsing, Raw encoding only |
| `auth.go` | VNC DES challenge-response (with the bit-reversed key quirk) |
| `decode.go` | Pixel buffer → text grid. Tries every (cell-size × font) combo; picks highest *exact* match rate on non-blank cells |
| `font.go` | Multiple PSF fonts: `uni2-fixed16` (Debian) + `seabios-vga-8x16` (QEMU/SeaBIOS, used by BIOS-mode OSes — slashed zero) |
| `bootstrap.go` | Anchor-string font bootstrapping for unknown OSes — finds known literals on screen, harvests (bitmap → rune) pairs with cross-validation |
| `render.go` | ANSI 24-bit colour renderer with diff vs previous frame; `RequestFull()` for resize-triggered redraw |
| `keys.go` | Stdin → X11 keysyms with Ctrl/Alt/Shift modifier wrapping; `shiftedASCII` map for QEMU VNC quirk |
| `picker.go` | `go-fzf` interactive VM picker over `hcloud server list -o json` |
| `dump.go` | PPM framebuffer dump for debugging |
| `render_test.go` | Renderer unit tests (initial draw, diff, RequestFull) |
| `web/` | noVNC v1.7.0 client (~1.7 MB), embedded via `//go:embed all:web` |
| `console_8x16.psf` | Debian's `Uni2-Fixed16.psf` (PSF1, 512 glyphs + Unicode table) |
| `vga_8x16.bin` | QEMU/SeaBIOS `vgafont16` (raw 256×16, CP437) |

## Build / test / run

```bash
go build -o ~/bin/hcloud-console .   # build + install
go test ./...                        # run renderer tests
hcloud-console <id>                  # terminal mode (default)
hcloud-console --novnc <id>          # browser mode
hcloud-console --select              # picker → terminal
```

Useful debug flags:
- `--debug FILE` — protocol/decode/SIGWINCH trace log
- `--once` — connect, render single frame as plain text, exit (good for piping `hcloud server request-console <id> | hcloud-console --from-stdin --once dummy`)
- `--send STRING` — scripted-input mode (escapes: `\n` `\t` `\b` `\e` `\^x` `\U \D \L \R` `\H \E`)
- `--dump-fb FILE` — PPM dump (with `--once` or `--send`)
- `--no-embedded-fonts` — skip embedded fonts, force the bootstrap path (for testing it stands alone)

## Things that bite you

1. **Exact-match scoring is critical.** Don't let nearest-neighbour
   matches contribute to the score used to choose a (cell-size × font)
   candidate — its leniency lets any candidate "match" anything.
   `decodeAt` only counts `decodeExact` toward the rate.

2. **Cell width is 8 px even when cells are 9 px.** VGA's 9th column
   is just a replication of the 8th for line-drawing characters.
   `decodeCell` always hashes on the first 8 columns regardless of
   `cw`, so the same font tables work for both 8×16 and 9×16 grids.

3. **QEMU's VNC server doesn't auto-shift.** Sending the keysym for
   `:` produces `;` in the guest. `keys.go:7` (`shiftedASCII`) maps
   shifted ASCII to base+Shift; capital letters are handled correctly
   by the kernel keymap and are deliberately NOT in that table.

4. **bubbletea (used by `go-fzf`) leaves terminal state behind.** On
   entering and exiting raw mode in `tty.go` we send `ansiHardReset`
   (alt-screen exit, mouse tracking off, cursor on, SGR reset, title
   clear). Don't simplify that bundle — each component is there for
   a reason discovered through testing.

5. **Path anchors leak.** `/dev/`, `/etc/`, `/usr/` etc. all have the
   shape `/X1X2X3/` with the same slash constraint, so their false
   matches reinforce each other's bogus claims. We only use word
   anchors with internal letter repeats (`bootstrap.go:strongAnchors`).

6. **Linux fbcon font ≠ IBM VGA ROM.** Debian uses `Uni2-Fixed16`
   (X.Org "Fixed", thin single-stroke); SeaBIOS uses its own (slashed
   zero, double-stroke, like classic IBM). Both need to be embedded.

## Test VMs

The user's hcloud project has a few standing VMs we use for testing:

- **`130241713` / `test2`** — Debian 13 ARM, password `test`. Default
  Linux fbcon at 1280×800, 8×16 cells. Used as the canonical Linux
  test target. Logged in already during the keyboard-bug session.
- **`25887908` / `pike`** — OpenBSD/amd64. BIOS text mode at 720×400,
  9×16 cells, slashed-zero font. We can read the login screen but
  don't have credentials.
- **`16288692` / `owl`**, **`25887900` / `fugu`**, **`128222175` /
  `pelican`** — production-ish VMs, don't disturb.

VM creation: cpx11 was deprecated; smallest x86 in EU is now cpx22.
ARM (cax11) is cheaper but uses UEFI/EDK2 and skips standard VGA
text mode, which defeats most of our testing.

## User preferences

- One-liner commit messages, no `Co-Authored-By: Claude` line.
- The user shells with fish.
- The user runs on macOS arm64.
- Don't reach for libraries when a bounded hand-rolled solution is
  simpler — applies especially to font/glyph matching where the
  problem size is tiny.

## Repo

- Public: <https://github.com/t-unix/hcloud-console>
- Default branch: `main`
- License: not yet specified (binary embeds noVNC under MPL-2.0 and
  the SeaBIOS/Debian fonts under their respective terms; documented
  in `README.md`).
