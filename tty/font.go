package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
)

// console_8x16.psf is the Linux PSF1 console font shipped by Debian's
// console-setup as "Uni2-Fixed16" (FONTFACE=Fixed FONTSIZE=8x16). It's an
// 8×16 bitmap font derived from the X.Org "fixed" family. PSF1 layout:
//
//	header    : 4 bytes (magic, mode, charsize)
//	glyphs    : numGlyphs × charsize bytes of bitmap (8 px wide, charsize tall)
//	unicode   : optional table mapping each glyph index → list of UCS-2
//	            codepoints (terminated by 0xFFFF, sequences by 0xFFFE)
//
// We only need the primary codepoint per glyph for our reverse lookup.
//
//go:embed console_8x16.psf
var psfData []byte

const (
	psf1Magic0      = 0x36
	psf1Magic1      = 0x04
	psf1Mode512     = 0x01
	psf1ModeHasTab  = 0x02
	psf1Separator   = 0xFFFE
	psf1Terminator  = 0xFFFF
	glyphCharHeight = 16
)

var (
	glyphBitmaps [][]byte
	glyphRune    []rune
	glyphIndex   map[fontKey]int
)

type fontKey [16]byte

func init() {
	if err := parsePSF(); err != nil {
		panic("embedded console font is corrupt: " + err.Error())
	}
}

func parsePSF() error {
	if len(psfData) < 4 || psfData[0] != psf1Magic0 || psfData[1] != psf1Magic1 {
		return fmt.Errorf("not a PSF1 file")
	}
	mode := psfData[2]
	charSize := int(psfData[3])
	if charSize != glyphCharHeight {
		return fmt.Errorf("expected %d-pixel-tall glyphs, got %d", glyphCharHeight, charSize)
	}

	numGlyphs := 256
	if mode&psf1Mode512 != 0 {
		numGlyphs = 512
	}

	bitmapStart := 4
	bitmapEnd := bitmapStart + numGlyphs*charSize
	if bitmapEnd > len(psfData) {
		return fmt.Errorf("PSF truncated: bitmap end %d > file size %d", bitmapEnd, len(psfData))
	}

	glyphBitmaps = make([][]byte, numGlyphs)
	for i := 0; i < numGlyphs; i++ {
		glyphBitmaps[i] = psfData[bitmapStart+i*charSize : bitmapStart+(i+1)*charSize]
	}

	glyphRune = make([]rune, numGlyphs)
	if mode&psf1ModeHasTab != 0 {
		if err := parseUnicodeTable(psfData[bitmapEnd:], numGlyphs); err != nil {
			return err
		}
	}

	glyphIndex = make(map[fontKey]int, numGlyphs)
	for i := 0; i < numGlyphs; i++ {
		var k fontKey
		copy(k[:], glyphBitmaps[i])
		if _, ok := glyphIndex[k]; !ok {
			glyphIndex[k] = i
		}
	}
	return nil
}

func parseUnicodeTable(data []byte, numGlyphs int) error {
	pos := 0
	for i := 0; i < numGlyphs; i++ {
		first := rune(0)
		for pos+1 < len(data) {
			c := binary.LittleEndian.Uint16(data[pos : pos+2])
			pos += 2
			if c == psf1Terminator {
				break
			}
			if c == psf1Separator {
				// Skip the rest of this glyph's sequences.
				for pos+1 < len(data) {
					c = binary.LittleEndian.Uint16(data[pos : pos+2])
					pos += 2
					if c == psf1Terminator {
						break
					}
				}
				break
			}
			if first == 0 {
				first = rune(c)
			}
		}
		glyphRune[i] = first
	}
	return nil
}

// matchGlyph returns the rune for an exact bitmap match.
func matchGlyph(k fontKey) (rune, bool) {
	idx, ok := glyphIndex[k]
	if !ok {
		return 0, false
	}
	r := glyphRune[idx]
	return r, r != 0
}

// nearestGlyph finds the glyph with the smallest hamming distance to k,
// trying small vertical baseline shifts. Used as a tolerance fallback when
// the framebuffer's font is structurally similar but not byte-identical.
func nearestGlyph(k fontKey, maxDist int) (rune, int, bool) {
	bestIdx := -1
	bestDist := 1 << 30
	for i := 0; i < len(glyphBitmaps); i++ {
		var blank, full = true, true
		for j := 0; j < glyphCharHeight; j++ {
			b := glyphBitmaps[i][j]
			if b != 0 {
				blank = false
			}
			if b != 0xFF {
				full = false
			}
		}
		if blank || full || glyphRune[i] == 0 {
			continue
		}
		d := minShiftedHamming(k, i, 3)
		if d < bestDist {
			bestDist = d
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestDist > maxDist {
		return 0, bestDist, false
	}
	return glyphRune[bestIdx], bestDist, true
}

func minShiftedHamming(k fontKey, glyphIdx, maxShift int) int {
	best := 1 << 30
	for s := -maxShift; s <= maxShift; s++ {
		d := shiftedHamming(k, glyphIdx, s)
		if d < best {
			best = d
		}
	}
	return best
}

func shiftedHamming(k fontKey, glyphIdx, shift int) int {
	d := 0
	g := glyphBitmaps[glyphIdx]
	for j := 0; j < glyphCharHeight; j++ {
		gj := j + shift
		var gb byte
		if gj >= 0 && gj < glyphCharHeight {
			gb = g[gj]
		}
		x := k[j] ^ gb
		x = x - ((x >> 1) & 0x55)
		x = (x & 0x33) + ((x >> 2) & 0x33)
		x = (x + (x >> 4)) & 0x0F
		d += int(x)
	}
	return d
}
