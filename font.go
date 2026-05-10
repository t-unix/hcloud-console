package main

import (
	_ "embed"
	"encoding/binary"
	"fmt"
)

// font represents one bitmap console font: a flat array of glyphs (each
// charHeight bytes, 8 pixels wide), plus the rune each glyph maps to and
// a reverse lookup keyed on the bitmap.
type font struct {
	name       string
	charHeight int
	glyphs     [][]byte // [glyph index][row byte]
	runes      []rune   // [glyph index]
	index      map[fontKey]int
}

type fontKey [16]byte

// console_8x16.psf is Debian's `Uni2-Fixed16` (FONTFACE=Fixed FONTSIZE=8x16),
// the default x.org-derived "Fixed" font Linux ships in console-setup.
//
//go:embed console_8x16.psf
var debianPSF []byte

// vga_8x16.bin is the SeaBIOS/QEMU `vgafont16` 8×16 bitmap (CP437) —
// the font QEMU's emulated VGA card actually renders into the
// framebuffer in text mode. Used by anything booting BIOS/SeaBIOS in
// Hetzner Cloud (OpenBSD, FreeBSD, NetBSD, BIOS-mode Linux, GRUB,
// SeaBIOS itself). Slashed-zero, double-stroke letterforms — matches
// what you see on screen, not what an IBM PC ROM has.
//
// 256 glyphs × 16 bytes/glyph = 4096 bytes, no Unicode table.
//
//go:embed vga_8x16.bin
var seabiosVGABin []byte

// fonts is the ordered list of bitmap fonts the decoder will try. The
// first that yields a high exact-match rate on the framebuffer wins.
var fonts []*font

func init() {
	dbn, err := loadPSF1("uni2-fixed16", debianPSF)
	if err != nil {
		panic("embedded debian PSF is corrupt: " + err.Error())
	}
	vga := loadCP437Raw("seabios-vga-8x16", seabiosVGABin)
	fonts = []*font{dbn, vga}
}

// loadPSF1 parses the PSF1 format Linux ships in /usr/share/consolefonts.
//
//	header    : 4 bytes (magic, mode, charsize)
//	glyphs    : numGlyphs × charsize bytes of bitmap (8 px wide)
//	unicode   : optional table mapping each glyph index → list of UCS-2
//	            codepoints (terminated by 0xFFFF, sequences by 0xFFFE)
func loadPSF1(name string, data []byte) (*font, error) {
	const (
		magic0     = 0x36
		magic1     = 0x04
		mode512    = 0x01
		modeHasTab = 0x02
		separator  = 0xFFFE
		terminator = 0xFFFF
	)
	if len(data) < 4 || data[0] != magic0 || data[1] != magic1 {
		return nil, fmt.Errorf("%s: not PSF1", name)
	}
	mode := data[2]
	charSize := int(data[3])
	if charSize != 16 {
		return nil, fmt.Errorf("%s: expected 16-row glyphs, got %d", name, charSize)
	}
	numGlyphs := 256
	if mode&mode512 != 0 {
		numGlyphs = 512
	}
	bmStart := 4
	bmEnd := bmStart + numGlyphs*charSize
	if bmEnd > len(data) {
		return nil, fmt.Errorf("%s: truncated", name)
	}

	f := &font{
		name:       name,
		charHeight: charSize,
		glyphs:     make([][]byte, numGlyphs),
		runes:      make([]rune, numGlyphs),
		index:      make(map[fontKey]int, numGlyphs),
	}
	for i := 0; i < numGlyphs; i++ {
		f.glyphs[i] = data[bmStart+i*charSize : bmStart+(i+1)*charSize]
	}
	if mode&modeHasTab != 0 {
		tab := data[bmEnd:]
		pos := 0
		for i := 0; i < numGlyphs; i++ {
			first := rune(0)
			for pos+1 < len(tab) {
				c := binary.LittleEndian.Uint16(tab[pos : pos+2])
				pos += 2
				if c == terminator {
					break
				}
				if c == separator {
					for pos+1 < len(tab) {
						c = binary.LittleEndian.Uint16(tab[pos : pos+2])
						pos += 2
						if c == terminator {
							break
						}
					}
					break
				}
				if first == 0 {
					first = rune(c)
				}
			}
			f.runes[i] = first
		}
	}
	for i := 0; i < numGlyphs; i++ {
		var k fontKey
		copy(k[:], f.glyphs[i])
		if _, ok := f.index[k]; !ok {
			f.index[k] = i
		}
	}
	return f, nil
}

// loadCP437Raw parses a raw 256-glyph × 16-byte font (no header, no
// Unicode table) using the codepage-437 mapping for runes.
func loadCP437Raw(name string, data []byte) *font {
	const numGlyphs = 256
	if len(data) != numGlyphs*16 {
		panic(fmt.Sprintf("%s: expected %d bytes, got %d", name, numGlyphs*16, len(data)))
	}
	f := &font{
		name:       name,
		charHeight: 16,
		glyphs:     make([][]byte, numGlyphs),
		runes:      make([]rune, numGlyphs),
		index:      make(map[fontKey]int, numGlyphs),
	}
	for i := 0; i < numGlyphs; i++ {
		f.glyphs[i] = data[i*16 : (i+1)*16]
		f.runes[i] = cp437ToRune[i]
	}
	for i := 0; i < numGlyphs; i++ {
		var k fontKey
		copy(k[:], f.glyphs[i])
		if _, ok := f.index[k]; !ok {
			f.index[k] = i
		}
	}
	return f
}

// matchExact returns the rune for a 1:1 bitmap match, or false.
func (f *font) matchExact(k fontKey) (rune, bool) {
	idx, ok := f.index[k]
	if !ok {
		return 0, false
	}
	r := f.runes[idx]
	return r, r != 0
}

// nearest finds the glyph with the smallest hamming distance to k,
// trying small vertical baseline shifts.
func (f *font) nearest(k fontKey, maxDist int) (rune, int, bool) {
	bestIdx := -1
	bestDist := 1 << 30
	for i := 0; i < len(f.glyphs); i++ {
		var blank, full = true, true
		for j := 0; j < f.charHeight; j++ {
			b := f.glyphs[i][j]
			if b != 0 {
				blank = false
			}
			if b != 0xFF {
				full = false
			}
		}
		if blank || full || f.runes[i] == 0 {
			continue
		}
		d := f.minShiftedHamming(k, i, 3)
		if d < bestDist {
			bestDist = d
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestDist > maxDist {
		return 0, bestDist, false
	}
	return f.runes[bestIdx], bestDist, true
}

func (f *font) minShiftedHamming(k fontKey, glyphIdx, maxShift int) int {
	best := 1 << 30
	for s := -maxShift; s <= maxShift; s++ {
		d := f.shiftedHamming(k, glyphIdx, s)
		if d < best {
			best = d
		}
	}
	return best
}

func (f *font) shiftedHamming(k fontKey, glyphIdx, shift int) int {
	d := 0
	g := f.glyphs[glyphIdx]
	for j := 0; j < f.charHeight; j++ {
		gj := j + shift
		var gb byte
		if gj >= 0 && gj < f.charHeight {
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

// cp437ToRune is the standard codepage 437 → Unicode mapping used to
// assign runes to glyphs in raw IBM VGA font dumps.
var cp437ToRune = [256]rune{
	0x0020, 0x263A, 0x263B, 0x2665, 0x2666, 0x2663, 0x2660, 0x2022,
	0x25D8, 0x25CB, 0x25D9, 0x2642, 0x2640, 0x266A, 0x266B, 0x263C,
	0x25BA, 0x25C4, 0x2195, 0x203C, 0x00B6, 0x00A7, 0x25AC, 0x21A8,
	0x2191, 0x2193, 0x2192, 0x2190, 0x221F, 0x2194, 0x25B2, 0x25BC,
	0x0020, 0x0021, 0x0022, 0x0023, 0x0024, 0x0025, 0x0026, 0x0027,
	0x0028, 0x0029, 0x002A, 0x002B, 0x002C, 0x002D, 0x002E, 0x002F,
	0x0030, 0x0031, 0x0032, 0x0033, 0x0034, 0x0035, 0x0036, 0x0037,
	0x0038, 0x0039, 0x003A, 0x003B, 0x003C, 0x003D, 0x003E, 0x003F,
	0x0040, 0x0041, 0x0042, 0x0043, 0x0044, 0x0045, 0x0046, 0x0047,
	0x0048, 0x0049, 0x004A, 0x004B, 0x004C, 0x004D, 0x004E, 0x004F,
	0x0050, 0x0051, 0x0052, 0x0053, 0x0054, 0x0055, 0x0056, 0x0057,
	0x0058, 0x0059, 0x005A, 0x005B, 0x005C, 0x005D, 0x005E, 0x005F,
	0x0060, 0x0061, 0x0062, 0x0063, 0x0064, 0x0065, 0x0066, 0x0067,
	0x0068, 0x0069, 0x006A, 0x006B, 0x006C, 0x006D, 0x006E, 0x006F,
	0x0070, 0x0071, 0x0072, 0x0073, 0x0074, 0x0075, 0x0076, 0x0077,
	0x0078, 0x0079, 0x007A, 0x007B, 0x007C, 0x007D, 0x007E, 0x2302,
	0x00C7, 0x00FC, 0x00E9, 0x00E2, 0x00E4, 0x00E0, 0x00E5, 0x00E7,
	0x00EA, 0x00EB, 0x00E8, 0x00EF, 0x00EE, 0x00EC, 0x00C4, 0x00C5,
	0x00C9, 0x00E6, 0x00C6, 0x00F4, 0x00F6, 0x00F2, 0x00FB, 0x00F9,
	0x00FF, 0x00D6, 0x00DC, 0x00A2, 0x00A3, 0x00A5, 0x20A7, 0x0192,
	0x00E1, 0x00ED, 0x00F3, 0x00FA, 0x00F1, 0x00D1, 0x00AA, 0x00BA,
	0x00BF, 0x2310, 0x00AC, 0x00BD, 0x00BC, 0x00A1, 0x00AB, 0x00BB,
	0x2591, 0x2592, 0x2593, 0x2502, 0x2524, 0x2561, 0x2562, 0x2556,
	0x2555, 0x2563, 0x2551, 0x2557, 0x255D, 0x255C, 0x255B, 0x2510,
	0x2514, 0x2534, 0x252C, 0x251C, 0x2500, 0x253C, 0x255E, 0x255F,
	0x255A, 0x2554, 0x2569, 0x2566, 0x2560, 0x2550, 0x256C, 0x2567,
	0x2568, 0x2564, 0x2565, 0x2559, 0x2558, 0x2552, 0x2553, 0x256B,
	0x256A, 0x2518, 0x250C, 0x2588, 0x2584, 0x258C, 0x2590, 0x2580,
	0x03B1, 0x00DF, 0x0393, 0x03C0, 0x03A3, 0x03C3, 0x00B5, 0x03C4,
	0x03A6, 0x0398, 0x03A9, 0x03B4, 0x221E, 0x03C6, 0x03B5, 0x2229,
	0x2261, 0x00B1, 0x2265, 0x2264, 0x2320, 0x2321, 0x00F7, 0x2248,
	0x00B0, 0x2219, 0x00B7, 0x221A, 0x207F, 0x00B2, 0x25A0, 0x00A0,
}
