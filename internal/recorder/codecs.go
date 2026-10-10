package recorder

import (
	"bytes"
	"crypto/cipher"
	"errors"
	"fmt"
)

// LiveKit's frame encryption (livekit-client FrameCryptor) leaves a codec-specific header in the
// clear, used as AES-GCM additional data, and appends `IV || IV length || key index`:
//
//	| clear header | ciphertext + tag | IV (12) | IV length (1) | key index (1) |
//
// For VP8 the clear header is 10 bytes on key frames (frame tag + start code + dimensions) and 3 on
// delta frames. The Go SDK ships Opus, H.264 and H.265 decryptors, but not VP8: this is that one.
const (
	vp8KeyFrameClearBytes   = 10
	vp8DeltaFrameClearBytes = 3
	frameTrailerBytes       = 2
)

var errFrameTooShort = errors.New("encrypted frame too short")

// vp8IsKeyFrame reads the VP8 frame tag: bit 0 of the first byte is 0 for key frames.
func vp8IsKeyFrame(frame []byte) bool {
	return len(frame) > 0 && frame[0]&0x01 == 0
}

// vp8Dimensions reads width and height from a key frame's uncompressed header.
func vp8Dimensions(frame []byte) (width, height uint16, ok bool) {
	if !vp8IsKeyFrame(frame) || len(frame) < 10 || !bytes.Equal(frame[3:6], []byte{0x9d, 0x01, 0x2a}) {
		return 0, 0, false
	}
	width = (uint16(frame[6]) | uint16(frame[7])<<8) & 0x3fff
	height = (uint16(frame[8]) | uint16(frame[9])<<8) & 0x3fff
	return width, height, true
}

// decryptVP8 matches e2ee.DecryptFunc. It returns (nil, nil) for server-injected frames.
func decryptVP8(frame, sifTrailer []byte, block cipher.Block) ([]byte, error) {
	if len(sifTrailer) > 0 && bytes.HasSuffix(frame, sifTrailer) {
		return nil, nil
	}
	clear := vp8DeltaFrameClearBytes
	if vp8IsKeyFrame(frame) {
		clear = vp8KeyFrameClearBytes
	}
	if len(frame) < clear+frameTrailerBytes {
		return nil, errFrameTooShort
	}
	ivLength := int(frame[len(frame)-frameTrailerBytes])
	ivStart := len(frame) - frameTrailerBytes - ivLength
	if ivLength == 0 || ivStart < clear {
		return nil, fmt.Errorf("bad IV length %d for a %d-byte frame", ivLength, len(frame))
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, ivLength)
	if err != nil {
		return nil, err
	}
	out := make([]byte, clear, len(frame))
	copy(out, frame[:clear])
	return gcm.Open(out, frame[ivStart:ivStart+ivLength], frame[clear:ivStart], frame[:clear])
}

// opusSamples is how many 48 kHz samples an Opus packet holds (RFC 6716 §3.1), or 0 when malformed.
func opusSamples(packet []byte) int {
	if len(packet) == 0 {
		return 0
	}
	toc := packet[0]
	config := int(toc >> 3)
	var per int // samples per frame at 48 kHz
	switch {
	case config < 12: // SILK: 10, 20, 40, 60 ms
		per = []int{480, 960, 1920, 2880}[config%4]
	case config < 16: // Hybrid: 10, 20 ms
		per = []int{480, 960}[config%2]
	default: // CELT: 2.5, 5, 10, 20 ms
		per = []int{120, 240, 480, 960}[config%4]
	}
	var frames int
	switch toc & 0x03 {
	case 0:
		frames = 1
	case 1, 2:
		frames = 2
	case 3:
		if len(packet) < 2 {
			return 0
		}
		frames = int(packet[1] & 0x3f)
	}
	return per * frames
}

// h264NALTypes lists the NAL unit types in an Annex B access unit.
func h264NALTypes(frame []byte) []byte {
	var types []byte
	for i := 0; i+3 < len(frame); i++ {
		if frame[i] != 0 || frame[i+1] != 0 {
			continue
		}
		switch {
		case frame[i+2] == 1:
			types = append(types, frame[i+3]&0x1f)
			i += 3
		case frame[i+2] == 0 && i+4 < len(frame) && frame[i+3] == 1:
			types = append(types, frame[i+4]&0x1f)
			i += 4
		}
	}
	return types
}

// h264IsKeyFrame: an access unit with an IDR slice (type 5).
func h264IsKeyFrame(frame []byte) bool {
	for _, t := range h264NALTypes(frame) {
		if t == 5 {
			return true
		}
	}
	return false
}

// h264Dimensions only checks that a key frame carries its SPS (type 7) so a decoder can start
// there; the size is left to the decoder, which reads it from the SPS.
func h264Dimensions(frame []byte) (width, height uint16, ok bool) {
	for _, t := range h264NALTypes(frame) {
		if t == 7 {
			return 0, 0, h264IsKeyFrame(frame)
		}
	}
	return 0, 0, false
}
