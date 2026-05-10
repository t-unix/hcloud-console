package main

import (
	"bufio"
	"fmt"
	"os"
)

// dumpPPM writes the current framebuffer pixel buffer to a binary PPM file
// (P6) so we can visually inspect what the server actually sent us.
func dumpPPM(rfb *rfbConn, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	bw := bufio.NewWriter(f)
	defer bw.Flush()

	fmt.Fprintf(bw, "P6\n%d %d\n255\n", rfb.width, rfb.height)
	row := make([]byte, rfb.width*3)
	for y := 0; y < rfb.height; y++ {
		for x := 0; x < rfb.width; x++ {
			off := (y*rfb.width + x) * 4
			row[x*3+0] = rfb.pixels[off+2] // R
			row[x*3+1] = rfb.pixels[off+1] // G
			row[x*3+2] = rfb.pixels[off+0] // B
		}
		bw.Write(row)
	}
	return nil
}
