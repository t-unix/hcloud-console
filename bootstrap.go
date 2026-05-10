package main

import (
	"fmt"
	"sync"
)

// Bootstrap font discovery.
//
// When none of the embedded fonts match the framebuffer well (a guest OS
// using a font we don't have a copy of), we try to learn the (bitmap →
// rune) mapping directly from the screen by looking for well-known
// strings — "checking", "running", "Password:", "/dev/", … — which
// appear on virtually every Unix boot screen.
//
// Only anchors with a *repeated letter* are used, because they impose a
// positional constraint (cells at the repeated indices must have
// pixel-identical bitmaps) that random text almost never satisfies. So
// even a single match of "checking" gives us 8 high-confidence (bitmap →
// rune) pairs.
//
// We additionally cross-validate by accumulating votes across anchors:
// a (bitmap, rune) pair is accepted only if the rune is the clear
// winner for that bitmap. If two anchors disagree on what a given
// bitmap means, the bitmap is dropped.

// anchor is a literal string with at least one repeated letter. The
// repeats list is precomputed: each entry is a pair (i, j) where
// anchor[i] == anchor[j], i < j.
type anchor struct {
	s       string
	repeats [][2]int
}

func newAnchor(s string) anchor {
	a := anchor{s: s}
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[i] == s[j] && s[i] != ' ' {
				a.repeats = append(a.repeats, [2]int{i, j})
			}
		}
	}
	return a
}

// strongAnchors are common Unix boot/login phrases. Each has at least
// one repeated letter — their positions enforce a constraint that
// rejects almost all false matches in random text.
var strongAnchors = func() []anchor {
	literals := []string{
		// Login / auth
		"password:", "Password:",
		// OS identifiers with internal repeats
		"FreeBSD",   // ee
		"Alpine Linux",
		"Linux kernel",
		// Boot messages
		"checking", // cc... (c at 0, 3)
		"starting", // tt (1, 4)
		"running",  // nn adjacent
		"kernel",   // ee (1, 4)
		"loading",
		"systemd",
		"reading",
		"reached",  // ee
		"command",  // mm adjacent
		"completed",
		"connection",
		"continuing",
		"daemons",
		"directory",
		"enabled",
		"failed",
		"finished",
		"initialized",
		"installed",
		"looking",   // oo adjacent
		"mounted",
		"network",
		"received",  // ee
		"sending",
		"service",   // ee (1, 6)
		"services",
		"settings",  // tt adjacent
		"started",   // tt (1, 4)
		"starting",
		"stopped",   // pp adjacent
		"stopping",  // pp adjacent
		"succeeded", // cc adjacent, ee adjacent
		"system",
		"timeout",
		"waiting",
		// Common short words with strong repeats — fill in 'o', 'l'
		// coverage that the longer anchors above tend to miss.
		"boot",     // 'oo'
		"root",     // 'oo'
		"good",     // 'oo'
		"look",     // 'oo'
		"pool",     // 'oo'
		"tool",     // 'oo'
		"local",    // l..l
		"global",   // l..l
		"savecore", // 'e' at 3, 7
		"follow",   // ll + oo
		"logging",  // gg
		// Path anchors deliberately omitted: they're all the
		// "/X1X2X3/" shape so their false matches reinforce each
		// other's bogus claims about the slash bitmap.
	}
	out := make([]anchor, 0, len(literals))
	seen := map[string]bool{}
	for _, s := range literals {
		if seen[s] {
			continue
		}
		seen[s] = true
		a := newAnchor(s)
		if len(a.repeats) == 0 {
			continue // skip anchors that don't actually have repeats
		}
		out = append(out, a)
	}
	return out
}()

var (
	bootstrapMu    sync.Mutex
	bootstrapDone  bool
	bootstrapFonts []*font
)

// bootstrapFontsFor returns the discovered fonts (one per cell-size
// where anchors were found). On first call it scans the framebuffer
// for anchor strings and caches the result.
func bootstrapFontsFor(rfb *rfbConn, candidates []cellSize) []*font {
	bootstrapMu.Lock()
	defer bootstrapMu.Unlock()
	if bootstrapDone {
		return bootstrapFonts
	}
	for _, c := range candidates {
		if rfb.width%c.cw != 0 || rfb.height%c.ch != 0 {
			continue
		}
		if f := tryBootstrap(rfb, c.cw, c.ch); f != nil {
			bootstrapFonts = append(bootstrapFonts, f)
			debug("bootstrap font %s discovered %d glyphs at %dx%d cells",
				f.name, len(f.glyphs), c.cw, c.ch)
		}
	}
	bootstrapDone = true
	return bootstrapFonts
}

func resetBootstrap() {
	bootstrapMu.Lock()
	bootstrapDone = false
	bootstrapFonts = nil
	bootstrapMu.Unlock()
}

// tryBootstrap walks every cell at (cw, ch), finds every consistent
// occurrence of every strong anchor, accumulates votes for each
// (bitmap, rune) pair, and returns a font built from the clear
// winners.
func tryBootstrap(rfb *rfbConn, cw, ch int) *font {
	cells := extractCellBitmaps(rfb, cw, ch)

	// votes[bitmap][rune] = total weight from all anchor matches that
	// claimed this bitmap means this rune.
	// sources[bitmap] = set of anchor literals that contributed votes
	// for this bitmap.
	// anchorHits[anchor.s] = how many times this anchor matched in the
	// frame. Multi-hit anchors are far more likely to be real (a false
	// match is a random independent event; getting two false matches
	// of the same anchor is much rarer).
	votes := make(map[fontKey]map[rune]int)
	sources := make(map[fontKey]map[string]bool)
	anchorHits := make(map[string]int)

	matches := 0
	for _, a := range strongAnchors {
		ms := findAllAnchorMatches(cells, a)
		anchorHits[a.s] = len(ms)
		if len(ms) > 0 {
			debug("  anchor %q: %d matches (repeats=%d)", a.s, len(ms), len(a.repeats))
		}
		for _, m := range ms {
			matches++
			weight := 1 + len(a.repeats)
			for k, r := range m {
				if votes[k] == nil {
					votes[k] = make(map[rune]int)
					sources[k] = make(map[string]bool)
				}
				votes[k][r] += weight
				sources[k][a.s] = true
			}
		}
	}

	// Accept a (bitmap → rune) discovery if the rune is the strict
	// majority winner AND any of:
	//   * ≥2 distinct anchors agree, OR
	//   * a high-confidence anchor (2+ internal repeats) agrees, OR
	//   * a single source matched ≥2 times (multi-hit anchors are
	//     overwhelmingly likely to be real).
	discovered := make(map[fontKey]rune)
	for bitmap, runes := range votes {
		var bestRune rune
		var bestCount, total int
		for r, c := range runes {
			total += c
			if c > bestCount {
				bestCount = c
				bestRune = r
			}
		}
		if bestCount*2 <= total {
			continue
		}
		nSrcs := len(sources[bitmap])
		highConf := false
		multiHit := false
		for src := range sources[bitmap] {
			if anchorHits[src] >= 2 {
				multiHit = true
			}
			for _, a := range strongAnchors {
				if a.s == src && len(a.repeats) >= 2 {
					highConf = true
				}
			}
		}
		if nSrcs >= 2 || highConf || multiHit {
			discovered[bitmap] = bestRune
		}
	}
	debug("tryBootstrap %dx%d: %d anchor matches, %d glyphs accepted",
		cw, ch, matches, len(discovered))
	if len(discovered) < 5 {
		return nil
	}
	return buildAutoFont(fmt.Sprintf("auto-%dx%d", cw, ch), discovered)
}

// extractCellBitmaps returns the 1-bit fg-mask of every cell at the
// given cell size. Blank or non-text cells are returned as the zero key.
func extractCellBitmaps(rfb *rfbConn, cw, ch int) [][]fontKey {
	cols := rfb.width / cw
	rows := rfb.height / ch
	out := make([][]fontKey, rows)
	for r := 0; r < rows; r++ {
		out[r] = make([]fontKey, cols)
		for c := 0; c < cols; c++ {
			out[r][c] = cellBitmapAt(rfb, c*cw, r*ch, cw, ch)
		}
	}
	return out
}

// cellBitmapAt is decodeCell minus the rune lookup — returns just the
// 16-byte fg-mask, or zero for blank/ambiguous cells.
func cellBitmapAt(rfb *rfbConn, x, y, cw, ch int) fontKey {
	freq := make(map[uint32]int, 4)
	for j := 0; j < ch; j++ {
		for i := 0; i < cw; i++ {
			freq[rfb.Pixel(x+i, y+j)]++
		}
	}
	if len(freq) < 2 {
		return fontKey{}
	}
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
	_ = bg
	if bgCount+fgCount < cw*ch-1 {
		return fontKey{}
	}
	var key fontKey
	for j := 0; j < ch && j < 16; j++ {
		var row byte
		for i := 0; i < 8 && i < cw; i++ {
			if rfb.Pixel(x+i, y+j) == fg {
				row |= 1 << (7 - i)
			}
		}
		key[j] = row
	}
	return key
}

// findAllAnchorMatches returns every (bitmap → rune) mapping that the
// anchor produces when matched at any row × starting position with
// fully consistent cells.
func findAllAnchorMatches(cells [][]fontKey, a anchor) []map[fontKey]rune {
	n := len(a.s)
	if n == 0 {
		return nil
	}
	var out []map[fontKey]rune
	for r := 0; r < len(cells); r++ {
		row := cells[r]
		if len(row) < n {
			continue
		}
		for start := 0; start <= len(row)-n; start++ {
			// Cheap pre-check: every repeat-letter constraint must
			// hold before bothering with the full match. Also reject
			// when the cells *immediately around* the anchor look
			// like they're part of a longer word — i.e. the cell
			// before start (or after start+n) is non-blank — because
			// real anchors are word-bounded by space or punctuation.
			ok := true
			for _, p := range a.repeats {
				if row[start+p[0]] != row[start+p[1]] {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			var blank fontKey
			if start > 0 && row[start-1] != blank {
				continue
			}
			if start+n < len(row) && row[start+n] != blank {
				continue
			}
			if m := tryAnchorAt(row, start, a.s); m != nil {
				out = append(out, m)
				debug("    match %q at row %d col %d", a.s, r, start)
			}
		}
	}
	return out
}

// tryAnchorAt checks whether the cells in row[start..start+len(s)] are
// consistent with the anchor literal s: same letter → same bitmap,
// same bitmap → same letter, spaces in s → blank cells.
func tryAnchorAt(row []fontKey, start int, s string) map[fontKey]rune {
	var blank fontKey
	runeToBitmap := make(map[rune]fontKey, 8)
	bitmapToRune := make(map[fontKey]rune, 8)
	for i, ch := range s {
		cell := row[start+i]
		isSpace := ch == ' '
		isBlank := cell == blank
		if isSpace != isBlank {
			return nil
		}
		if isSpace {
			continue
		}
		if existing, ok := runeToBitmap[ch]; ok {
			if existing != cell {
				return nil
			}
		} else {
			runeToBitmap[ch] = cell
		}
		if existing, ok := bitmapToRune[cell]; ok {
			if existing != ch {
				return nil
			}
		} else {
			bitmapToRune[cell] = ch
		}
	}
	return bitmapToRune
}

func buildAutoFont(name string, mapping map[fontKey]rune) *font {
	f := &font{
		name:       name,
		charHeight: 16,
		glyphs:     make([][]byte, 0, len(mapping)),
		runes:      make([]rune, 0, len(mapping)),
		index:      make(map[fontKey]int, len(mapping)),
	}
	for k, r := range mapping {
		idx := len(f.glyphs)
		b := make([]byte, 16)
		copy(b, k[:])
		f.glyphs = append(f.glyphs, b)
		f.runes = append(f.runes, r)
		f.index[k] = idx
	}
	return f
}

type cellSize struct{ cw, ch int }
