package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type pixelFormat struct {
	bitsPerPixel uint8
	depth        uint8
	bigEndian    uint8
	trueColor    uint8
	redMax       uint16
	greenMax     uint16
	blueMax      uint16
	redShift     uint8
	greenShift   uint8
	blueShift    uint8
}

type rfbConn struct {
	conn   net.Conn
	wsConn *websocket.Conn
	ctx    context.Context

	width, height int
	pf            pixelFormat
	name          string

	// Pixel buffer in 32bpp little-endian BGRA layout (4 bytes/pixel).
	pixels []byte

	writeMu sync.Mutex
	closeMu sync.Mutex
	closed  bool
}

func dialRFB(ctx context.Context, wsURL, password string) (*rfbConn, error) {
	c, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		Subprotocols: []string{"binary"},
	})
	if err != nil {
		return nil, fmt.Errorf("websocket dial: %w", err)
	}
	c.SetReadLimit(64 << 20) // framebuffer updates can be large
	debug("ws connected, subprotocol=%q, status=%d", c.Subprotocol(), resp.StatusCode)

	r := &rfbConn{
		ctx:    ctx,
		wsConn: c,
		conn:   websocket.NetConn(ctx, c, websocket.MessageBinary),
	}
	if err := r.handshake(password); err != nil {
		_ = c.Close(websocket.StatusInternalError, "handshake failed")
		return nil, err
	}
	return r, nil
}

func (r *rfbConn) Close() error {
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.wsConn.Close(websocket.StatusNormalClosure, "")
}

func (r *rfbConn) handshake(password string) error {
	// 1. ProtocolVersion: server → client, then client → server.
	// Reflect the server's version (clamped to 3.8 on the high end) so we
	// don't break compatibility with older noVNC servers.
	var ver [12]byte
	if _, err := io.ReadFull(r.conn, ver[:]); err != nil {
		return fmt.Errorf("read version: %w", err)
	}
	if string(ver[:4]) != "RFB " {
		return fmt.Errorf("unexpected version banner: %q", ver)
	}
	debug("server version: %q", string(ver[:]))
	reply := "RFB 003.008\n"
	// Versions older than 3.8 use a slightly different security flow; we
	// support 3.7 too. Anything we don't recognise, we ask for 3.8.
	if string(ver[:11]) == "RFB 003.007" {
		reply = "RFB 003.007\n"
	}
	if _, err := r.conn.Write([]byte(reply)); err != nil {
		return fmt.Errorf("write version: %w", err)
	}
	debug("client version: %q", reply)

	// 2. Security handshake (RFB 3.8)
	var secCount [1]byte
	if _, err := io.ReadFull(r.conn, secCount[:]); err != nil {
		return fmt.Errorf("read security count: %w", err)
	}
	if secCount[0] == 0 {
		return r.readFailureReason("server refused connection")
	}
	secTypes := make([]byte, secCount[0])
	if _, err := io.ReadFull(r.conn, secTypes); err != nil {
		return fmt.Errorf("read security types: %w", err)
	}
	debug("security types: %v", secTypes)

	var picked byte
	// Prefer VNC auth (2) since Hetzner gives us a password.
	for _, s := range secTypes {
		if s == 2 {
			picked = 2
			break
		}
	}
	if picked == 0 {
		for _, s := range secTypes {
			if s == 1 {
				picked = 1
				break
			}
		}
	}
	if picked == 0 {
		return fmt.Errorf("no supported security types: %v", secTypes)
	}
	debug("picked security type: %d", picked)
	if _, err := r.conn.Write([]byte{picked}); err != nil {
		return fmt.Errorf("send security choice: %w", err)
	}

	if picked == 2 {
		var challenge [16]byte
		if _, err := io.ReadFull(r.conn, challenge[:]); err != nil {
			return fmt.Errorf("read challenge: %w", err)
		}
		resp := vncAuthResponse(challenge[:], password)
		if _, err := r.conn.Write(resp); err != nil {
			return fmt.Errorf("send vnc response: %w", err)
		}
	}

	var secResult [4]byte
	if _, err := io.ReadFull(r.conn, secResult[:]); err != nil {
		return fmt.Errorf("read security result: %w", err)
	}
	if binary.BigEndian.Uint32(secResult[:]) != 0 {
		return r.readFailureReason("authentication failed")
	}

	// 3. ClientInit (shared = 1)
	if _, err := r.conn.Write([]byte{1}); err != nil {
		return fmt.Errorf("send clientinit: %w", err)
	}

	// 4. ServerInit
	var hdr [24]byte
	if _, err := io.ReadFull(r.conn, hdr[:]); err != nil {
		return fmt.Errorf("read serverinit: %w", err)
	}
	r.width = int(binary.BigEndian.Uint16(hdr[0:2]))
	r.height = int(binary.BigEndian.Uint16(hdr[2:4]))
	debug("serverinit: %dx%d bpp=%d depth=%d be=%d truecolor=%d",
		r.width, r.height, hdr[4], hdr[5], hdr[6], hdr[7])
	r.pf = pixelFormat{
		bitsPerPixel: hdr[4],
		depth:        hdr[5],
		bigEndian:    hdr[6],
		trueColor:    hdr[7],
		redMax:       binary.BigEndian.Uint16(hdr[8:10]),
		greenMax:     binary.BigEndian.Uint16(hdr[10:12]),
		blueMax:      binary.BigEndian.Uint16(hdr[12:14]),
		redShift:     hdr[14],
		greenShift:   hdr[15],
		blueShift:    hdr[16],
	}
	// Bytes 17-19 are padding inside the 16-byte server-pixel-format,
	// then bytes 20-23 are the desktop-name length.
	nl := binary.BigEndian.Uint32(hdr[20:24])
	debug("desktop name length: %d", nl)
	if nl > 1<<16 {
		return fmt.Errorf("implausible desktop-name length %d", nl)
	}
	name := make([]byte, nl)
	if _, err := io.ReadFull(r.conn, name); err != nil {
		return fmt.Errorf("read name: %w", err)
	}
	r.name = string(name)
	debug("desktop name: %q", r.name)

	// Force a 32-bit BGRX little-endian format we can decode trivially.
	want := pixelFormat{
		bitsPerPixel: 32, depth: 24, bigEndian: 0, trueColor: 1,
		redMax: 255, greenMax: 255, blueMax: 255,
		redShift: 16, greenShift: 8, blueShift: 0,
	}
	if err := r.setPixelFormat(want); err != nil {
		return fmt.Errorf("set pixel format: %w", err)
	}
	r.pf = want

	// Encodings: only Raw is mandatory; we ask for DesktopSize too so we
	// can resize if the server changes resolution (rare for cloud consoles).
	const encRaw = 0
	const encDesktopSize = -223
	if err := r.setEncodings([]int32{encRaw, encDesktopSize}); err != nil {
		return fmt.Errorf("set encodings: %w", err)
	}

	r.pixels = make([]byte, r.width*r.height*4)
	return nil
}

func (r *rfbConn) readFailureReason(prefix string) error {
	var rl [4]byte
	if _, err := io.ReadFull(r.conn, rl[:]); err != nil {
		return fmt.Errorf("%s (no reason)", prefix)
	}
	reason := make([]byte, binary.BigEndian.Uint32(rl[:]))
	_, _ = io.ReadFull(r.conn, reason)
	return fmt.Errorf("%s: %s", prefix, reason)
}

func (r *rfbConn) write(buf []byte) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	_, err := r.conn.Write(buf)
	return err
}

func (r *rfbConn) setPixelFormat(pf pixelFormat) error {
	buf := make([]byte, 20)
	buf[0] = 0 // SetPixelFormat
	buf[4] = pf.bitsPerPixel
	buf[5] = pf.depth
	buf[6] = pf.bigEndian
	buf[7] = pf.trueColor
	binary.BigEndian.PutUint16(buf[8:10], pf.redMax)
	binary.BigEndian.PutUint16(buf[10:12], pf.greenMax)
	binary.BigEndian.PutUint16(buf[12:14], pf.blueMax)
	buf[14] = pf.redShift
	buf[15] = pf.greenShift
	buf[16] = pf.blueShift
	return r.write(buf)
}

func (r *rfbConn) setEncodings(enc []int32) error {
	buf := make([]byte, 4+4*len(enc))
	buf[0] = 2 // SetEncodings
	binary.BigEndian.PutUint16(buf[2:4], uint16(len(enc)))
	for i, e := range enc {
		binary.BigEndian.PutUint32(buf[4+4*i:], uint32(e))
	}
	return r.write(buf)
}

func (r *rfbConn) requestUpdate(incremental bool) error {
	var inc byte
	if incremental {
		inc = 1
	}
	buf := make([]byte, 10)
	buf[0] = 3 // FramebufferUpdateRequest
	buf[1] = inc
	binary.BigEndian.PutUint16(buf[2:4], 0)
	binary.BigEndian.PutUint16(buf[4:6], 0)
	binary.BigEndian.PutUint16(buf[6:8], uint16(r.width))
	binary.BigEndian.PutUint16(buf[8:10], uint16(r.height))
	return r.write(buf)
}

// keyEvent sends a single KeyEvent message.
func (r *rfbConn) keyEvent(keysym uint32, down bool) error {
	var d byte
	if down {
		d = 1
	}
	buf := make([]byte, 8)
	buf[0] = 4
	buf[1] = d
	binary.BigEndian.PutUint32(buf[4:8], keysym)
	return r.write(buf)
}

// readMessageType blocks for the next server message and returns its 1-byte
// type code.
func (r *rfbConn) readMessageType() (byte, error) {
	var t [1]byte
	if _, err := io.ReadFull(r.conn, t[:]); err != nil {
		return 0, err
	}
	return t[0], nil
}

// readFramebufferUpdate handles a FramebufferUpdate message. It reads each
// rectangle and writes Raw pixels into r.pixels. Returns the rectangles
// actually applied (so the caller can decide which cells to redecode).
func (r *rfbConn) readFramebufferUpdate() ([]rect, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r.conn, hdr[:]); err != nil {
		return nil, fmt.Errorf("read fbu header: %w", err)
	}
	n := int(binary.BigEndian.Uint16(hdr[1:3]))
	rects := make([]rect, 0, n)
	for i := 0; i < n; i++ {
		var rh [12]byte
		if _, err := io.ReadFull(r.conn, rh[:]); err != nil {
			return nil, fmt.Errorf("read rect %d header: %w", i, err)
		}
		x := int(binary.BigEndian.Uint16(rh[0:2]))
		y := int(binary.BigEndian.Uint16(rh[2:4]))
		w := int(binary.BigEndian.Uint16(rh[4:6]))
		h := int(binary.BigEndian.Uint16(rh[6:8]))
		enc := int32(binary.BigEndian.Uint32(rh[8:12]))

		switch enc {
		case 0: // Raw
			if err := r.readRaw(x, y, w, h); err != nil {
				return nil, fmt.Errorf("rect %d raw: %w", i, err)
			}
			rects = append(rects, rect{x, y, w, h})
		case -223: // DesktopSize pseudo-encoding
			r.width, r.height = w, h
			r.pixels = make([]byte, w*h*4)
			rects = append(rects, rect{0, 0, w, h})
		default:
			return nil, fmt.Errorf("unsupported encoding %d (request only Raw)", enc)
		}
	}
	return rects, nil
}

type rect struct{ x, y, w, h int }

func (r *rfbConn) readRaw(x, y, w, h int) error {
	row := make([]byte, w*4)
	for j := 0; j < h; j++ {
		if _, err := io.ReadFull(r.conn, row); err != nil {
			return err
		}
		off := ((y+j)*r.width + x) * 4
		copy(r.pixels[off:off+w*4], row)
	}
	return nil
}

// drainOther consumes a non-FramebufferUpdate server message. Most are
// uninteresting for our use case.
func (r *rfbConn) drainOther(msgType byte) error {
	switch msgType {
	case 1: // SetColourMapEntries
		var hdr [5]byte
		if _, err := io.ReadFull(r.conn, hdr[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint16(hdr[3:5]))
		entries := make([]byte, n*6)
		_, err := io.ReadFull(r.conn, entries)
		return err
	case 2: // Bell — no body
		return nil
	case 3: // ServerCutText
		var hdr [7]byte
		if _, err := io.ReadFull(r.conn, hdr[:]); err != nil {
			return err
		}
		n := int(binary.BigEndian.Uint32(hdr[3:7]))
		buf := make([]byte, n)
		_, err := io.ReadFull(r.conn, buf)
		return err
	default:
		return fmt.Errorf("unknown server message type %d", msgType)
	}
}

// Pixel returns the BGRX pixel at (x, y) packed into uint32 0x00RRGGBB.
func (r *rfbConn) Pixel(x, y int) uint32 {
	off := (y*r.width + x) * 4
	// our pf is BGRX little-endian (B at 0, G at 1, R at 2, X at 3)
	b := uint32(r.pixels[off+0])
	g := uint32(r.pixels[off+1])
	rd := uint32(r.pixels[off+2])
	return (rd << 16) | (g << 8) | b
}

// withTimeout sets a read deadline and returns a func to clear it.
func (r *rfbConn) withTimeout(d time.Duration) func() {
	_ = r.conn.SetReadDeadline(time.Now().Add(d))
	return func() { _ = r.conn.SetReadDeadline(time.Time{}) }
}
