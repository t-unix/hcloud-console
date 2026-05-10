package main

import "crypto/des"

// vncAuthResponse implements the VNC password DES challenge-response.
// Each byte of the password key has its bits reversed before being used
// as the DES key — a long-standing quirk of the RFB protocol.
func vncAuthResponse(challenge []byte, password string) []byte {
	pw := []byte(password)
	key := make([]byte, 8)
	for i := 0; i < 8; i++ {
		var b byte
		if i < len(pw) {
			b = pw[i]
		}
		key[i] = bitReverse(b)
	}
	block, err := des.NewCipher(key)
	if err != nil {
		panic(err)
	}
	resp := make([]byte, 16)
	block.Encrypt(resp[0:8], challenge[0:8])
	block.Encrypt(resp[8:16], challenge[8:16])
	return resp
}

func bitReverse(b byte) byte {
	b = (b&0xF0)>>4 | (b&0x0F)<<4
	b = (b&0xCC)>>2 | (b&0x33)<<2
	b = (b&0xAA)>>1 | (b&0x55)<<1
	return b
}
