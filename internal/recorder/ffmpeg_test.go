package recorder

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/pkg/samplebuilder"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
)

// TestRealVP8RoundTrip runs a real VP8 stream through LiveKit-style encryption, our decryptor and
// our IVF writer. Set VP8_IVF to an IVF file (e.g. from ffmpeg's libvpx); the output lands in
// VP8_OUT_DIR (or a temp dir) for ffprobe.
func TestRealVP8RoundTrip(t *testing.T) {
	in := os.Getenv("VP8_IVF")
	if in == "" {
		t.Skip("VP8_IVF not set")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	derived, _ := lksdk.DeriveKeyFromBytes([]byte("matrix-media-key"))
	block, _ := aes.NewCipher(derived)
	kp := e2ee.NewExternalKeyProvider()
	_ = kp.SetRawKey(derived, 3)
	dec := e2ee.NewGCMFrameDecryptor(kp, decryptVP8, nil)

	dir := os.Getenv("VP8_OUT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	sink, err := newIVFSink(filepath.Join(dir, "out.ivf"), vp8Codec)
	if err != nil {
		t.Fatal(err)
	}
	frames := 0
	for off := 32; off+12 <= len(data); frames++ {
		size := int(binary.LittleEndian.Uint32(data[off:]))
		pts := binary.LittleEndian.Uint64(data[off+4:])
		frame := data[off+12 : off+12+size]
		off += 12 + size
		clear := vp8DeltaFrameClearBytes
		if vp8IsKeyFrame(frame) {
			clear = vp8KeyFrameClearBytes
		}
		plain, err := dec.DecryptFrame(encryptLikeLiveKit(t, block, frame, clear, 3))
		if err != nil {
			t.Fatalf("frame %d: %v", frames, err)
		}
		if err = sink.WriteFrame(plain, uint32(pts*3000)); err != nil { // 30 fps in 90 kHz ticks
			t.Fatal(err)
		}
	}
	if err = sink.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d frames", frames)
}

// TestRealH264RoundTrip: set H264_ANNEXB to a raw Annex B stream (one access unit per AUD).
func TestRealH264RoundTrip(t *testing.T) {
	in := os.Getenv("H264_ANNEXB")
	if in == "" {
		t.Skip("H264_ANNEXB not set")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	derived, _ := lksdk.DeriveKeyFromBytes([]byte("matrix-media-key"))
	block, _ := aes.NewCipher(derived)
	kp := e2ee.NewExternalKeyProvider()
	_ = kp.SetRawKey(derived, 3)
	dec := e2ee.NewGCMFrameDecryptor(kp, e2ee.DecryptGCMH264SampleCustomCipher, nil)
	dir := os.Getenv("VP8_OUT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	sink, err := newIVFSink(filepath.Join(dir, "out.h264.ivf"), h264Codec)
	if err != nil {
		t.Fatal(err)
	}
	// Split on access unit delimiters (00 00 00 01 09).
	aud := []byte{0, 0, 0, 1, 9}
	var units [][]byte
	for start := 0; start < len(data); {
		next := bytes.Index(data[start+len(aud):], aud)
		if next < 0 {
			units = append(units, data[start:])
			break
		}
		units = append(units, data[start:start+len(aud)+next])
		start += len(aud) + next
	}
	for i, au := range units {
		enc, err := e2ee.EncryptGCMH264SampleCustomCipher(au, 3, block)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := dec.DecryptFrame(enc)
		if err != nil {
			t.Fatalf("unit %d: %v", i, err)
		}
		if !bytes.Equal(plain, au) {
			t.Fatalf("unit %d differs after decryption", i)
		}
		if err = sink.WriteFrame(plain, uint32(i*3000)); err != nil {
			t.Fatal(err)
		}
	}
	if err = sink.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d access units", len(units))
}

// TestRealH264ThroughRTP is the recording path end to end, minus the network: access units are
// encrypted like livekit-client does, packetized like a browser (SPS/PPS in a STAP-A, slices in
// FU-As), then go through the sample builder, the unit assembler, the decryptor and the IVF writer.
func TestRealH264ThroughRTP(t *testing.T) {
	in := os.Getenv("H264_ANNEXB")
	if in == "" {
		t.Skip("H264_ANNEXB not set")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	derived, _ := lksdk.DeriveKeyFromBytes([]byte("matrix-media-key"))
	block, _ := aes.NewCipher(derived)
	kp := e2ee.NewExternalKeyProvider()
	_ = kp.SetRawKey(derived, 3)
	dec := e2ee.NewGCMFrameDecryptor(kp, e2ee.DecryptGCMH264SampleCustomCipher, nil)

	dir := os.Getenv("VP8_OUT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	sink, err := newIVFSink(filepath.Join(dir, "rtp.h264.ivf"), h264Codec)
	if err != nil {
		t.Fatal(err)
	}
	written := 0
	units := &unitAssembler{emit: func(frame []byte, ts uint32) {
		plain, err := dec.DecryptFrame(frame)
		if err != nil {
			t.Fatalf("decrypt unit at ts %d: %v", ts, err)
		}
		if err = sink.WriteFrame(plain, ts); err != nil {
			t.Fatal(err)
		}
		written++
	}}
	sb := samplebuilder.New(maxLate, &codecs.H264Packet{}, 90000)
	payloader := &codecs.H264Payloader{}

	seq := uint16(1000)
	for i, au := range splitAccessUnits(data) {
		enc, err := e2ee.EncryptGCMH264SampleCustomCipher(au, 3, block)
		if err != nil {
			t.Fatal(err)
		}
		payloads := payloader.Payload(1200, enc)
		for j, p := range payloads {
			sb.Push(&rtp.Packet{
				Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: uint32(i * 3000), Marker: j == len(payloads)-1},
				Payload: p,
			})
			seq++
			for {
				sample, ts := sb.PopWithTimestamp()
				if sample == nil {
					break
				}
				units.push(sample.Data, ts, sample.PrevDroppedPackets > 0)
			}
		}
	}
	for sample, ts := sb.ForcePopWithTimestamp(); sample != nil; sample, ts = sb.ForcePopWithTimestamp() {
		units.push(sample.Data, ts, false)
	}
	units.flush()
	if err = sink.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d access units written", written)
}

// splitAccessUnits cuts an Annex B stream at its access unit delimiters and drops them, and
// normalises start codes to 4 bytes: what libwebrtc hands to the frame encryptor, and what the RTP
// depacketizer produces. (x264 writes some 3-byte ones, which would change the authenticated bytes.)
func splitAccessUnits(data []byte) [][]byte {
	aud := []byte{0, 0, 0, 1, 9}
	var units [][]byte
	for _, chunk := range bytes.Split(fourByteStartCodes(data), aud) {
		if len(chunk) < 2 {
			continue
		}
		// Drop the rest of the AUD NAL (its one-byte payload) up to the next start code.
		if i := bytes.Index(chunk, []byte{0, 0, 0, 1}); i >= 0 {
			units = append(units, chunk[i:])
		}
	}
	return units
}

func fourByteStartCodes(au []byte) []byte {
	out := make([]byte, 0, len(au)+16)
	for i := 0; i < len(au); i++ {
		if i+2 < len(au) && au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 && (i == 0 || au[i-1] != 0) {
			out = append(out, 0, 0, 0, 1)
			i += 2
			continue
		}
		out = append(out, au[i])
	}
	return out
}
