package main

import (
	"bufio"
	"errors"
	"io"
	"time"
)

// X11 keysyms we care about.
const (
	ksBackspace = 0xff08
	ksTab       = 0xff09
	ksReturn    = 0xff0d
	ksEscape    = 0xff1b
	ksInsert    = 0xff63
	ksDelete    = 0xffff
	ksHome      = 0xff50
	ksEnd       = 0xff57
	ksPageUp    = 0xff55
	ksPageDown  = 0xff56
	ksLeft      = 0xff51
	ksUp        = 0xff52
	ksRight     = 0xff53
	ksDown      = 0xff54
	ksControlL  = 0xffe3
	ksAltL      = 0xffe9
	ksShiftL    = 0xffe1
	ksF1        = 0xffbe // through F12 = 0xffc9
)

// errExit is returned by readKey when the user hits the escape sequence
// configured to leave the wrapper (Ctrl+]).
var errExit = errors.New("user requested exit")

// pumpKeys reads from stdin in raw mode and forwards events to the RFB
// connection. Returns when the user exits or stdin closes.
func pumpKeys(rfb *rfbConn, stdin io.Reader) error {
	br := bufio.NewReader(stdin)
	for {
		ev, err := readKey(br)
		if err == errExit {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sendKey(rfb, ev); err != nil {
			return err
		}
	}
}

type keyEvent struct {
	keysym uint32
	ctrl   bool
	alt    bool
	shift  bool
}

func readKey(br *bufio.Reader) (keyEvent, error) {
	b, err := br.ReadByte()
	if err != nil {
		return keyEvent{}, err
	}
	switch b {
	case 0x1d: // Ctrl+]
		return keyEvent{}, errExit
	case 0x1b: // ESC — start of sequence or lone Escape
		return readEscape(br)
	case 0x7f, 0x08:
		return keyEvent{keysym: ksBackspace}, nil
	case '\r', '\n':
		return keyEvent{keysym: ksReturn}, nil
	case '\t':
		return keyEvent{keysym: ksTab}, nil
	}
	if b < 0x20 {
		// Control character: send as Ctrl + letter.
		// 0x01 → 'a', 0x02 → 'b', etc.
		return keyEvent{keysym: uint32('a' + b - 1), ctrl: true}, nil
	}
	// Printable ASCII.
	return keyEvent{keysym: uint32(b)}, nil
}

// readEscape is called after we've consumed a 0x1b. It distinguishes a
// bare Escape keypress from a CSI/SS3 sequence using a short read deadline.
func readEscape(br *bufio.Reader) (keyEvent, error) {
	// Peek with a short timeout: if no follow-up byte arrives within
	// ~30ms, treat as bare Escape. This requires a deadline-capable
	// reader; bufio doesn't have one natively, so we approximate by
	// trying a non-blocking peek loop.
	deadline := time.Now().Add(30 * time.Millisecond)
	for time.Now().Before(deadline) {
		if br.Buffered() > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if br.Buffered() == 0 {
		return keyEvent{keysym: ksEscape}, nil
	}

	next, err := br.ReadByte()
	if err != nil {
		return keyEvent{keysym: ksEscape}, nil
	}

	switch next {
	case '[':
		return readCSI(br)
	case 'O':
		return readSS3(br)
	default:
		// ESC + char usually means Alt+char.
		return keyEvent{keysym: uint32(next), alt: true}, nil
	}
}

func readCSI(br *bufio.Reader) (keyEvent, error) {
	// Parse: parameters (digits/;) followed by a final byte (@-~).
	var params []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			return keyEvent{}, err
		}
		if b >= '0' && b <= '9' || b == ';' {
			params = append(params, b)
			continue
		}
		// final byte
		switch b {
		case 'A':
			return keyEvent{keysym: ksUp}, nil
		case 'B':
			return keyEvent{keysym: ksDown}, nil
		case 'C':
			return keyEvent{keysym: ksRight}, nil
		case 'D':
			return keyEvent{keysym: ksLeft}, nil
		case 'H':
			return keyEvent{keysym: ksHome}, nil
		case 'F':
			return keyEvent{keysym: ksEnd}, nil
		case '~':
			return tildeCSI(params), nil
		}
		// Unknown CSI — ignore.
		return keyEvent{keysym: 0}, nil
	}
}

func readSS3(br *bufio.Reader) (keyEvent, error) {
	b, err := br.ReadByte()
	if err != nil {
		return keyEvent{}, err
	}
	switch b {
	case 'P':
		return keyEvent{keysym: ksF1}, nil
	case 'Q':
		return keyEvent{keysym: ksF1 + 1}, nil
	case 'R':
		return keyEvent{keysym: ksF1 + 2}, nil
	case 'S':
		return keyEvent{keysym: ksF1 + 3}, nil
	case 'A':
		return keyEvent{keysym: ksUp}, nil
	case 'B':
		return keyEvent{keysym: ksDown}, nil
	case 'C':
		return keyEvent{keysym: ksRight}, nil
	case 'D':
		return keyEvent{keysym: ksLeft}, nil
	case 'H':
		return keyEvent{keysym: ksHome}, nil
	case 'F':
		return keyEvent{keysym: ksEnd}, nil
	}
	return keyEvent{keysym: 0}, nil
}

func tildeCSI(params []byte) keyEvent {
	switch string(params) {
	case "2":
		return keyEvent{keysym: ksInsert}
	case "3":
		return keyEvent{keysym: ksDelete}
	case "5":
		return keyEvent{keysym: ksPageUp}
	case "6":
		return keyEvent{keysym: ksPageDown}
	case "15":
		return keyEvent{keysym: ksF1 + 4} // F5
	case "17":
		return keyEvent{keysym: ksF1 + 5} // F6
	case "18":
		return keyEvent{keysym: ksF1 + 6}
	case "19":
		return keyEvent{keysym: ksF1 + 7}
	case "20":
		return keyEvent{keysym: ksF1 + 8}
	case "21":
		return keyEvent{keysym: ksF1 + 9}
	case "23":
		return keyEvent{keysym: ksF1 + 10}
	case "24":
		return keyEvent{keysym: ksF1 + 11} // F12
	}
	return keyEvent{keysym: 0}
}

// shiftedASCII maps shifted printable ASCII keysyms to (unshifted base,
// "needs Shift" implied). QEMU's VNC server interprets each keysym as if
// it were a US-layout physical key — so sending the keysym for ':' makes
// the guest see ';', not ':'. Wrap these in a Shift_L press/release.
//
// Capital letters (A-Z) are deliberately NOT in this table: QEMU's VNC
// pipeline does add Shift for them automatically via the kernel keymap,
// and synthesising another Shift would actually break ones like Ctrl+A.
var shiftedASCII = map[uint32]uint32{
	'!': '1', '@': '2', '#': '3', '$': '4', '%': '5',
	'^': '6', '&': '7', '*': '8', '(': '9', ')': '0',
	'_': '-', '+': '=',
	'{': '[', '}': ']', '|': '\\',
	':': ';', '"': '\'',
	'<': ',', '>': '.', '?': '/',
	'~': '`',
}

// sendKey delivers a keyEvent to the RFB server, wrapping the keysym in
// modifier press/release pairs as needed.
func sendKey(rfb *rfbConn, ev keyEvent) error {
	if ev.keysym == 0 {
		return nil
	}
	if base, ok := shiftedASCII[ev.keysym]; ok && !ev.shift && !ev.ctrl && !ev.alt {
		ev.shift = true
		ev.keysym = base
	}
	if ev.ctrl {
		if err := rfb.keyEvent(ksControlL, true); err != nil {
			return err
		}
	}
	if ev.alt {
		if err := rfb.keyEvent(ksAltL, true); err != nil {
			return err
		}
	}
	if ev.shift {
		if err := rfb.keyEvent(ksShiftL, true); err != nil {
			return err
		}
	}
	if err := rfb.keyEvent(ev.keysym, true); err != nil {
		return err
	}
	if err := rfb.keyEvent(ev.keysym, false); err != nil {
		return err
	}
	if ev.shift {
		if err := rfb.keyEvent(ksShiftL, false); err != nil {
			return err
		}
	}
	if ev.alt {
		if err := rfb.keyEvent(ksAltL, false); err != nil {
			return err
		}
	}
	if ev.ctrl {
		if err := rfb.keyEvent(ksControlL, false); err != nil {
			return err
		}
	}
	return nil
}
