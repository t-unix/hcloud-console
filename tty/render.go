package main

import (
	"fmt"
	"io"
	"strings"
)

type renderer struct {
	out      io.Writer
	prev     *grid
	curFG    uint32
	curBG    uint32
	cursorOK bool
}

const (
	ansiReset      = "\x1b[0m"
	ansiClear      = "\x1b[2J\x1b[H"
	ansiHideCursor = "\x1b[?25l"
	ansiShowCursor = "\x1b[?25h"
)

func newRenderer(out io.Writer) *renderer {
	return &renderer{
		out:   out,
		curFG: 0xFFFFFFFF, // sentinel "unset"
		curBG: 0xFFFFFFFF,
	}
}

func (r *renderer) reset() {
	fmt.Fprint(r.out, ansiReset+ansiClear)
	r.prev = nil
	r.curFG = 0xFFFFFFFF
	r.curBG = 0xFFFFFFFF
}

func (r *renderer) draw(g *grid) {
	if g == nil {
		return
	}
	full := r.prev == nil ||
		r.prev.cols != g.cols || r.prev.rows != g.rows
	if full {
		r.reset()
	}

	var b strings.Builder
	b.WriteString(ansiHideCursor)

	cursorRow, cursorCol := -1, -1
	for row := 0; row < g.rows; row++ {
		// Move once per row at the first changed cell.
		moved := false
		for col := 0; col < g.cols; col++ {
			cl := g.cells[row][col]
			if cl.cursor {
				cursorRow, cursorCol = row, col
			}
			if !full {
				prev := r.prev.cells[row][col]
				if prev == cl {
					moved = false
					continue
				}
			}
			if !moved || full && col == 0 {
				fmt.Fprintf(&b, "\x1b[%d;%dH", row+1, col+1)
				moved = true
			}
			r.applyColors(&b, cl.fg, cl.bg)
			if cl.ch == 0 {
				b.WriteRune(' ')
			} else {
				b.WriteRune(cl.ch)
			}
		}
		if full {
			b.WriteString("\r\n")
		}
	}

	if cursorRow >= 0 {
		fmt.Fprintf(&b, "\x1b[%d;%dH", cursorRow+1, cursorCol+1)
		b.WriteString(ansiShowCursor)
		r.cursorOK = true
	} else {
		// Park cursor below the grid; keep it hidden so it doesn't blink
		// in the middle of a redraw.
		fmt.Fprintf(&b, "\x1b[%d;1H", g.rows+1)
		r.cursorOK = false
	}
	b.WriteString(ansiReset)
	r.curFG, r.curBG = 0xFFFFFFFF, 0xFFFFFFFF // ansiReset cleared SGR

	_, _ = io.WriteString(r.out, b.String())
	r.prev = g
}

// drawNotice is shown when the frame is in graphical mode (matchRate too
// low to plausibly decode as text). We blank our output and tell the user.
func (r *renderer) drawNotice(rfb *rfbConn, g *grid) {
	r.reset()
	rate := 0.0
	cw, ch := 0, 0
	if g != nil {
		rate, cw, ch = g.matchRate, g.cellW, g.cellH
	}
	fmt.Fprintf(r.out,
		"\r\n  framebuffer is %dx%d, no text grid detected\r\n"+
			"  (best guess: %dx%d cells, %.0f%% match)\r\n"+
			"  the VM is probably in graphical mode; open the browser console\r\n"+
			"  press Ctrl+] to exit\r\n",
		rfb.width, rfb.height, cw, ch, rate*100)
	r.prev = nil
}

func (r *renderer) applyColors(b *strings.Builder, fg, bg uint32) {
	if fg != r.curFG {
		fmt.Fprintf(b, "\x1b[38;2;%d;%d;%dm",
			(fg>>16)&0xff, (fg>>8)&0xff, fg&0xff)
		r.curFG = fg
	}
	if bg != r.curBG {
		fmt.Fprintf(b, "\x1b[48;2;%d;%d;%dm",
			(bg>>16)&0xff, (bg>>8)&0xff, bg&0xff)
		r.curBG = bg
	}
}
