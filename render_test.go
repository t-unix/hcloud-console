package main

import (
	"bytes"
	"strings"
	"testing"
)

// makeGrid builds a small grid filled from rows: each string becomes one
// row of cells with the given fg/bg colours. Useful for tests.
func makeGrid(rows []string, fg, bg uint32) *grid {
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	g := &grid{
		cellW: 8, cellH: 16,
		cols: cols,
		rows: len(rows),
		cells: make([][]cell, len(rows)),
	}
	for ri, r := range rows {
		g.cells[ri] = make([]cell, cols)
		for ci := 0; ci < cols; ci++ {
			ch := byte(' ')
			if ci < len(r) {
				ch = r[ci]
			}
			g.cells[ri][ci] = cell{ch: rune(ch), fg: fg, bg: bg}
		}
	}
	return g
}

// gridCopy makes a deep copy so we can mutate one grid without affecting
// another (the renderer's diff compares cell values).
func gridCopy(g *grid) *grid {
	out := *g
	out.cells = make([][]cell, g.rows)
	for r := 0; r < g.rows; r++ {
		out.cells[r] = make([]cell, g.cols)
		copy(out.cells[r], g.cells[r])
	}
	return &out
}

func TestDraw_FirstFrameIsFull(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf)

	g := makeGrid([]string{"hello", "world"}, 0xFFFFFF, 0x000000)
	r.draw(g)

	out := buf.String()
	if !strings.Contains(out, ansiClear) {
		t.Fatalf("first frame should start with ansiClear (\\x1b[2J\\x1b[H); got %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("first frame should write 'hello'; got %q", out)
	}
	if !strings.Contains(out, "world") {
		t.Fatalf("first frame should write 'world'; got %q", out)
	}
}

func TestDraw_IdenticalSecondFrameIsNearlyEmpty(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf)

	g := makeGrid([]string{"hello", "world"}, 0xFFFFFF, 0x000000)
	r.draw(g)
	buf.Reset()
	r.draw(gridCopy(g)) // same content, different pointer

	out := buf.String()
	if strings.Contains(out, "hello") || strings.Contains(out, "world") {
		t.Fatalf("identical second frame should not re-render content; got %q", out)
	}
	if strings.Contains(out, ansiClear) {
		t.Fatalf("identical second frame should not clear screen; got %q", out)
	}
}

func TestRequestFull_TriggersFullRedraw(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf)

	g := makeGrid([]string{"hello", "world"}, 0xFFFFFF, 0x000000)
	r.draw(g)            // first frame — full
	r.draw(gridCopy(g))  // second frame — diff (nothing)
	buf.Reset()

	r.RequestFull() // SIGWINCH simulator

	out := buf.String()
	if !strings.Contains(out, ansiClear) {
		t.Fatalf("RequestFull should clear screen; got %q", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("RequestFull should re-render 'hello'; got %q", out)
	}
	if !strings.Contains(out, "world") {
		t.Fatalf("RequestFull should re-render 'world'; got %q", out)
	}
}

func TestRequestFull_NoLastGridIsNoop(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf)

	r.RequestFull() // never drew anything before

	if buf.Len() != 0 {
		t.Fatalf("RequestFull with no prior draw should write nothing; got %q",
			buf.String())
	}
}

func TestRequestFull_PaintsLatestGridNotInitial(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf)

	g1 := makeGrid([]string{"first"}, 0xFFFFFF, 0)
	g2 := makeGrid([]string{"secnd"}, 0xFFFFFF, 0)
	r.draw(g1)
	r.draw(g2)
	buf.Reset()

	r.RequestFull()

	out := buf.String()
	if !strings.Contains(out, "secnd") {
		t.Fatalf("RequestFull should paint the most recent grid; got %q", out)
	}
	if strings.Contains(out, "first") {
		t.Fatalf("RequestFull should NOT paint older grids; got %q", out)
	}
}

// TestDraw_ChangedCellOnlyEmitsThatCell verifies the diff path works:
// changing a single cell between two frames should result in output that
// contains the new char but not the unchanged ones.
func TestDraw_ChangedCellOnlyEmitsThatCell(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf)

	g := makeGrid(make20x5(), 0xFFFFFF, 0)
	r.draw(g)
	buf.Reset()

	g2 := gridCopy(g)
	g2.cells[2][10] = cell{ch: 'X', fg: 0xFFFFFF, bg: 0}
	r.draw(g2)

	out := buf.String()
	if !strings.Contains(out, "X") {
		t.Fatalf("changed cell should emit the new rune; got %q", out)
	}
	// On a 20×5 grid the full frame is hundreds of bytes; a single
	// cell change should be well under a tenth of that.
	full := makeFullFrameSize(t, g)
	if buf.Len() > full/2 {
		t.Fatalf("diff frame should be substantially smaller than full (full=%d, diff=%d)",
			full, buf.Len())
	}
}

func make20x5() []string {
	row := strings.Repeat("abcdefghij", 2)
	return []string{row, row, row, row, row}
}

func makeFullFrameSize(t *testing.T, g *grid) int {
	var buf bytes.Buffer
	r := newRenderer(&buf)
	r.draw(g)
	return buf.Len()
}
