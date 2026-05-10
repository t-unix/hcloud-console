package main

// Decoding the framebuffer pixel buffer back to a text grid.
//
// Strategy: try every (cell-size × font) combination, decode the whole
// frame with each, and pick the one with the highest *exact* match rate
// on non-blank cells. Nearest-neighbour matching is then used at render
// time as a tolerance fallback for individual cells; it never influences
// the cell-size or font choice (otherwise its leniency would let any
// candidate "match" anything).

type cell struct {
	ch     rune
	fg, bg uint32 // 0x00RRGGBB
	cursor bool   // text cursor visible in this cell
}

type grid struct {
	cellW, cellH int
	cols, rows   int
	cells        [][]cell // [row][col]
	font         *font    // chosen font
	matchRate    float64  // exact-match rate on non-blank cells (0..1)
}

// decode tries known cell-size candidates with each available font and
// returns the best fit. Returns nil if nothing fits.
//
// Strategy: try every (cell-size × embedded-font) combination and pick
// the highest exact-match rate on non-blank cells. If none of the
// embedded fonts is a great fit, fall back to a font *learned* from the
// framebuffer itself by anchor-string discovery (bootstrap.go).
func decode(rfb *rfbConn) *grid {
	w, h := rfb.width, rfb.height
	candidates := []cellSize{
		{9, 16}, // BIOS / OpenBSD / DOS — 720×400 etc.
		{8, 16}, // Linux fbcon
		{8, 8},
	}
	var best *grid
	tryFont := func(f *font) {
		for _, c := range candidates {
			if w%c.cw != 0 || h%c.ch != 0 {
				continue
			}
			g := decodeAt(rfb, c.cw, c.ch, f)
			if best == nil || g.matchRate > best.matchRate {
				best = g
			}
		}
	}

	if !*flagNoFonts {
		for _, f := range fonts {
			tryFont(f)
			if best != nil && best.matchRate >= 0.99 {
				return best
			}
		}
	}

	// Embedded fonts didn't fit perfectly — try bootstrapping a font
	// from anchor strings on screen.
	if best == nil || best.matchRate < 0.95 {
		for _, f := range bootstrapFontsFor(rfb, candidates) {
			tryFont(f)
		}
	}
	return best
}

func decodeAt(rfb *rfbConn, cw, ch int, f *font) *grid {
	cols := rfb.width / cw
	rows := rfb.height / ch
	g := &grid{
		cellW: cw, cellH: ch,
		cols:  cols,
		rows:  rows,
		cells: make([][]cell, rows),
		font:  f,
	}
	exactNonBlank := 0
	totalNonBlank := 0
	for r := 0; r < rows; r++ {
		g.cells[r] = make([]cell, cols)
		for c := 0; c < cols; c++ {
			cl, status := decodeCell(rfb, c*cw, r*ch, cw, ch, f)
			g.cells[r][c] = cl
			if status != decodeBlank {
				totalNonBlank++
				if status == decodeExact {
					exactNonBlank++
				}
			}
		}
	}
	// Score: fraction of non-blank cells that match a glyph exactly. A
	// fully blank screen scores 1.0 (nothing to disprove).
	if totalNonBlank == 0 {
		g.matchRate = 1.0
	} else {
		g.matchRate = float64(exactNonBlank) / float64(totalNonBlank)
	}
	return g
}

// decodeStatus tells the caller how confident a cell decode was.
type decodeStatus int

const (
	decodeBlank decodeStatus = iota // uniform colour — counts toward neither
	decodeExact                     // bitmap matched a glyph 1:1 (or via cursor variant)
	decodeApprox                    // matched via nearest-neighbour (lower confidence)
	decodeFail                      // no glyph match at all
)

func decodeCell(rfb *rfbConn, x, y, cw, ch int, f *font) (cell, decodeStatus) {
	// Tally pixel frequencies. Text-mode framebuffers use a small palette.
	freq := make(map[uint32]int, 4)
	for j := 0; j < ch; j++ {
		for i := 0; i < cw; i++ {
			freq[rfb.Pixel(x+i, y+j)]++
		}
	}

	if len(freq) == 1 {
		var c uint32
		for k := range freq {
			c = k
		}
		return cell{ch: ' ', fg: c, bg: c}, decodeBlank
	}

	// Identify bg (most common) and fg (second most).
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
	if bgCount+fgCount < totalPixels-1 {
		// More than a couple stray pixels in other colours — not text.
		return cell{ch: '?', fg: fg, bg: bg}, decodeFail
	}

	// Build the 16-byte bitmap (or shorter for ch<16; pad with zeros).
	var key fontKey
	for j := 0; j < ch && j < 16; j++ {
		var row byte
		// Hash on first 8 columns even when cw is 9 — VGA's 9th column
		// is just a replication of the 8th for line drawing characters
		// (or empty for everything else).
		for i := 0; i < 8 && i < cw; i++ {
			if rfb.Pixel(x+i, y+j) == fg {
				row |= 1 << (7 - i)
			}
		}
		key[j] = row
	}

	if r, ok := f.matchExact(key); ok {
		return cell{ch: r, fg: fg, bg: bg}, decodeExact
	}

	// Try cursor overlay: bottom two rows forced to fg (underscore cursor).
	if ch >= 16 && key[14] == 0xFF && key[15] == 0xFF {
		try := key
		try[14], try[15] = 0, 0
		if r, ok := f.matchExact(try); ok {
			return cell{ch: r, fg: fg, bg: bg, cursor: true}, decodeExact
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
		return cell{ch: ' ', fg: bg, bg: fg, cursor: true}, decodeExact
	}

	// Fallback: nearest-neighbour match (font tolerance for slight
	// baseline shifts and similar variants). Does NOT contribute to the
	// match rate used to choose between fonts.
	if r, _, ok := f.nearest(key, 24); ok {
		return cell{ch: r, fg: fg, bg: bg}, decodeApprox
	}

	return cell{ch: '?', fg: fg, bg: bg}, decodeFail
}
