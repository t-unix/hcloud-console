package main

// Decoding the framebuffer pixel buffer back to a text grid.
//
// Strategy: assume the frame is a regular grid of monospaced cells drawn
// with a known VGA font. For each cell we identify the two dominant pixel
// colors (bg = most common, fg = second), build a 16-byte 1-bit bitmap by
// classifying each pixel as fg or bg, and look the bitmap up in the
// pre-hashed VGA font table. If most cells match, we have text.

type cell struct {
	ch     rune
	fg, bg uint32 // 0x00RRGGBB
	cursor bool   // text cursor visible in this cell
}

type grid struct {
	cellW, cellH int
	cols, rows   int
	cells        [][]cell // [row][col]
	matchRate    float64  // 0..1, fraction of cells that decoded cleanly
}

// decode tries known cell-size candidates and returns the best decode.
// Returns nil if no candidate plausibly fits or match rate is too low.
func decode(rfb *rfbConn) *grid {
	w, h := rfb.width, rfb.height
	candidates := []struct{ cw, ch int }{
		{8, 16},
		{9, 16},
		{8, 8},
	}
	var best *grid
	for _, c := range candidates {
		if w%c.cw != 0 || h%c.ch != 0 {
			continue
		}
		g := decodeAt(rfb, c.cw, c.ch)
		if best == nil || g.matchRate > best.matchRate {
			best = g
		}
		if g.matchRate > 0.97 {
			break
		}
	}
	return best
}

func decodeAt(rfb *rfbConn, cw, ch int) *grid {
	cols := rfb.width / cw
	rows := rfb.height / ch
	g := &grid{
		cellW: cw, cellH: ch,
		cols:  cols,
		rows:  rows,
		cells: make([][]cell, rows),
	}
	matched := 0
	total := 0
	for r := 0; r < rows; r++ {
		g.cells[r] = make([]cell, cols)
		for c := 0; c < cols; c++ {
			cl, ok := decodeCell(rfb, c*cw, r*ch, cw, ch)
			g.cells[r][c] = cl
			total++
			if ok {
				matched++
			}
		}
	}
	if total > 0 {
		g.matchRate = float64(matched) / float64(total)
	}
	return g
}

// decodeCell decodes a single cell at pixel offset (x, y).
//
// The cell's "ok" flag is true when we found an exact font glyph match
// (or the cell is uniformly one color, treated as space).
func decodeCell(rfb *rfbConn, x, y, cw, ch int) (cell, bool) {
	// Tally pixel frequencies. VGA text mode has only 16 colors so this
	// is bounded.
	freq := make(map[uint32]int, 4)
	for j := 0; j < ch; j++ {
		for i := 0; i < cw; i++ {
			freq[rfb.Pixel(x+i, y+j)]++
		}
	}

	// Solid cell → blank.
	if len(freq) == 1 {
		var c uint32
		for k := range freq {
			c = k
		}
		return cell{ch: ' ', fg: c, bg: c}, true
	}

	// Identify bg (most common) and fg (second most). Reject if there
	// are more than 2 distinct colors AND the third+ have non-trivial
	// counts — that indicates anti-aliased graphics, not text.
	var bg, fg uint32
	var bgCount, fgCount int
	for c, n := range freq {
		if n > bgCount {
			fg, fgCount = bg, bgCount
			bg, bgCount = c, n
		} else if n > fgCount {
			fg, fgCount = c, n
		}
	}
	totalPixels := cw * ch
	dominant := bgCount + fgCount
	if dominant < totalPixels-1 {
		// More than a couple stray pixels in other colors.
		return cell{ch: '?', fg: fg, bg: bg}, false
	}

	// Build the 16-byte bitmap (or shorter for ch<16; pad with zeros).
	var key fontKey
	for j := 0; j < ch && j < 16; j++ {
		var row byte
		// Hash on first 8 columns even when cw is 9 — VGA's 9th column
		// is just a replication of the 8th for line drawing characters.
		for i := 0; i < 8 && i < cw; i++ {
			if rfb.Pixel(x+i, y+j) == fg {
				row |= 1 << (7 - i)
			}
		}
		key[j] = row
	}

	if r, ok := matchGlyph(key); ok {
		debugCell(x, y, key, r, 0, "exact")
		return cell{ch: r, fg: fg, bg: bg}, true
	}

	// Try cursor overlay: bottom two rows forced to fg (underscore cursor).
	if ch >= 16 && key[14] == 0xFF && key[15] == 0xFF {
		try := key
		try[14], try[15] = 0, 0
		if r, ok := matchGlyph(try); ok {
			return cell{ch: r, fg: fg, bg: bg, cursor: true}, true
		}
	}

	// Inverted-block cursor (every row 0xFF).
	allFG := true
	for j := 0; j < ch && j < 16; j++ {
		if key[j] != 0xFF {
			allFG = false
			break
		}
	}
	if allFG {
		return cell{ch: ' ', fg: bg, bg: fg, cursor: true}, true
	}

	// Fallback: nearest-neighbour match against IBM VGA. Other Linux
	// console fonts (X.Org "Fixed", Terminus, etc.) share enough shape
	// with IBM VGA that hamming distance below ~32 bits picks the right
	// codepoint with very few false positives.
	if r, d, ok := nearestGlyph(key, 32); ok {
		debugCell(x, y, key, r, d, "nearest")
		return cell{ch: r, fg: fg, bg: bg}, true
	}

	debugCell(x, y, key, '?', 0, "no-match")
	return cell{ch: '?', fg: fg, bg: bg}, false
}

func debugCell(x, y int, k fontKey, r rune, dist int, how string) {
	if debugLog == nil {
		return
	}
	debug("cell @(%d,%d) %s -> %q (dist=%d)", x, y, how, r, dist)
	for j := 0; j < 16; j++ {
		var bits string
		for i := 0; i < 8; i++ {
			if k[j]&(1<<(7-i)) != 0 {
				bits += "#"
			} else {
				bits += "."
			}
		}
		debug("  %s", bits)
	}
}
