package recorder

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/e2ee"
)

// encryptLikeLiveKit builds a frame the way livekit-client's FrameCryptor does.
func encryptLikeLiveKit(t *testing.T, block cipher.Block, frame []byte, clear int, kid byte) []byte {
	t.Helper()
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		t.Fatal(err)
	}
	out := append([]byte{}, frame[:clear]...)
	out = gcm.Seal(out, iv, frame[clear:], frame[:clear])
	out = append(out, iv...)
	return append(out, byte(len(iv)), kid)
}

func vp8KeyFrame(width, height uint16, payload []byte) []byte {
	f := []byte{0x10, 0x02, 0x00, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(f[6:], width)
	binary.LittleEndian.PutUint16(f[8:], height)
	return append(f, payload...)
}

func TestVP8DecryptsThroughTheSDKDecryptorWithMatrixKeys(t *testing.T) {
	// A Matrix media key goes through LiveKit's HKDF before use, exactly as Element Call does.
	raw := []byte("0123456789abcdef")
	derived, err := lksdk.DeriveKeyFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(derived)

	kp := e2ee.NewExternalKeyProvider()
	if err = kp.SetRawKey(derived, 7); err != nil {
		t.Fatal(err)
	}
	sif := []byte("LKROCKS")
	dec := e2ee.NewGCMFrameDecryptor(kp, decryptVP8, sif)

	key := vp8KeyFrame(640, 360, bytes.Repeat([]byte{0xaa}, 300))
	delta := append([]byte{0x31, 0x02, 0x00}, bytes.Repeat([]byte{0xbb}, 120)...)
	for name, tc := range map[string]struct {
		frame []byte
		clear int
	}{"key frame": {key, vp8KeyFrameClearBytes}, "delta frame": {delta, vp8DeltaFrameClearBytes}} {
		enc := encryptLikeLiveKit(t, block, tc.frame, tc.clear, 7)
		got, err := dec.DecryptFrame(enc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, tc.frame) {
			t.Fatalf("%s: decrypted frame differs", name)
		}
	}

	// A key index we have no key for fails instead of producing garbage.
	if _, err = dec.DecryptFrame(encryptLikeLiveKit(t, block, key, vp8KeyFrameClearBytes, 8)); err == nil {
		t.Fatal("decrypted with a missing key index")
	}
	// Server-injected frames are dropped.
	if got, err := dec.DecryptFrame(append([]byte{1, 2, 3}, sif...)); got != nil || err != nil {
		t.Fatalf("SIF frame: %v, %v", got, err)
	}
}

func TestVP8Dimensions(t *testing.T) {
	w, h, ok := vp8Dimensions(vp8KeyFrame(1280, 720, nil))
	if !ok || w != 1280 || h != 720 {
		t.Fatalf("got %dx%d ok=%v", w, h, ok)
	}
	if _, _, ok = vp8Dimensions([]byte{0x31, 0, 0, 0, 0, 0, 0, 0, 0, 0}); ok {
		t.Fatal("delta frame has no dimensions")
	}
}

func TestOpusSamples(t *testing.T) {
	for name, tc := range map[string]struct {
		packet []byte
		want   int
	}{
		"CELT 20 ms":       {[]byte{0xf8, 0xff, 0xfe}, 960},
		"SILK 20 ms":       {[]byte{0x08}, 960},
		"SILK 60 ms":       {[]byte{0x18}, 2880},
		"two CELT 10 ms":   {[]byte{0xf1}, 960},
		"code 3, 3x 20 ms": {[]byte{0xfb, 0x03}, 2880},
		"empty":            {nil, 0},
	} {
		if got := opusSamples(tc.packet); got != tc.want {
			t.Errorf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

func TestOggSinkFillsDTXGapsWithSilence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.ogg")
	sink, err := newOggSink(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := []byte{0xfc, 0x01, 0x02, 0x03} // CELT 20 ms
	// 20 ms, then a 1 s DTX gap, then 20 ms more: 48 960 + 960 samples in all.
	for _, ts := range []uint32{1000, 1960, 1960 + 960 + 48000} {
		if err = sink.WriteFrame(frame, ts); err != nil {
			t.Fatal(err)
		}
	}
	if err = sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := lastGranule(data); got != 960*3+48000 {
		t.Fatalf("final granule %d, want %d", got, 960*3+48000)
	}
}

// lastGranule finds the granule position of the last Ogg page.
func lastGranule(data []byte) uint64 {
	var granule uint64
	for i := 0; i+27 <= len(data); {
		if !bytes.Equal(data[i:i+4], []byte("OggS")) {
			i++
			continue
		}
		g := binary.LittleEndian.Uint64(data[i+6:])
		if g != ^uint64(0) {
			granule = g
		}
		segments := int(data[i+26])
		size := 27 + segments
		for _, s := range data[i+27 : i+27+segments] {
			size += int(s)
		}
		i += size
	}
	return granule
}

func TestIVFSinkStartsAtKeyFrameAndPatchesCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.ivf")
	sink, err := newIVFSink(path, vp8Codec)
	if err != nil {
		t.Fatal(err)
	}
	delta := []byte{0x31, 0x02, 0x00, 0xbb}
	key := vp8KeyFrame(320, 240, []byte{0xaa})
	for i, f := range [][]byte{delta, key, delta, delta} {
		if err = sink.WriteFrame(f, uint32(3000*i)); err != nil {
			t.Fatal(err)
		}
	}
	sink.Gap()
	_ = sink.WriteFrame(delta, 30000) // dropped: waiting for a key frame after the gap
	if err = sink.Close(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data[:4]) != "DKIF" || string(data[8:12]) != "VP80" {
		t.Fatal("bad IVF header")
	}
	if w, h := binary.LittleEndian.Uint16(data[12:]), binary.LittleEndian.Uint16(data[14:]); w != 320 || h != 240 {
		t.Fatalf("dimensions %dx%d", w, h)
	}
	// Three frames at 0, 3000, 6000: duration 6000 plus one average frame (3000).
	if d := binary.LittleEndian.Uint32(data[24:]); d != 9000 {
		t.Fatalf("duration %d, want 9000", d)
	}
	// Second frame's PTS is relative to the key frame: 3000 ticks of 90 kHz later.
	firstSize := binary.LittleEndian.Uint32(data[32:])
	second := 32 + 12 + int(firstSize)
	if pts := binary.LittleEndian.Uint64(data[second+4:]); pts != 3000 {
		t.Fatalf("second PTS %d, want 3000", pts)
	}
}
