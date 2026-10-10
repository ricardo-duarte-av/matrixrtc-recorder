package recorder

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
)

// frameSink writes one track's decrypted frames to a file.
type frameSink interface {
	// WriteFrame takes a whole frame and its RTP timestamp.
	WriteFrame(frame []byte, rtpTS uint32) error
	// Gap says frames were lost (or undecryptable): a video sink waits for the next key frame.
	Gap()
	// Started is true once something has been written.
	Started() bool
	Close() error
}

// tsUnwrapper turns 32-bit RTP timestamps into a monotonic 64-bit count from the first one.
type tsUnwrapper struct {
	started bool
	last    uint32
	total   int64
}

func (u *tsUnwrapper) unwrap(ts uint32) int64 {
	if !u.started {
		u.started, u.last = true, ts
		return 0
	}
	u.total += int64(int32(ts - u.last))
	u.last = ts
	return u.total
}

// oggSink writes Opus into Ogg. Element Call sends with DTX, so silence is a gap in RTP timestamps
// rather than packets; the gap is filled with Opus silence frames, or the recording would run fast.
type oggSink struct {
	w        *oggwriter.OggWriter
	ts       tsUnwrapper
	expected int64 // where the next packet should start, in samples
	started  bool
	// firstAt is the wall-clock time of the first frame, which PadTo measures from.
	firstAt time.Time
}

// opusSilence is a 20 ms CELT fullband mono silence frame.
var opusSilence = []byte{0xf8, 0xff, 0xfe}

const (
	opusSilenceSamples = 960
	maxSilenceFill     = 48000 * 60 * 10 // never fill more than 10 min at once
)

func newOggSink(path string) (*oggSink, error) {
	w, err := oggwriter.New(path, 48000, 2)
	if err != nil {
		return nil, err
	}
	return &oggSink{w: w}, nil
}

func (s *oggSink) WriteFrame(frame []byte, rtpTS uint32) error {
	pos := s.ts.unwrap(rtpTS)
	if s.started && pos > s.expected {
		gap := min(pos-s.expected, maxSilenceFill)
		for ; gap >= opusSilenceSamples; gap -= opusSilenceSamples {
			if err := s.write(opusSilence); err != nil {
				return err
			}
		}
	}
	if err := s.write(frame); err != nil {
		return err
	}
	if !s.started {
		s.firstAt = time.Now()
	}
	s.started = true
	s.expected = pos + int64(opusSamples(frame))
	return nil
}

func (s *oggSink) write(payload []byte) error {
	return s.w.WriteRTP(&rtp.Packet{Payload: payload})
}

// PadTo fills silence up to the wall-clock time end, for tracks that went quiet (muted) before it.
func (s *oggSink) PadTo(end time.Time) {
	if !s.started {
		return
	}
	target := end.Sub(s.firstAt).Milliseconds() * 48
	for gap := min(target-s.expected, maxSilenceFill); gap >= opusSilenceSamples; gap -= opusSilenceSamples {
		if s.write(opusSilence) != nil {
			return
		}
		s.expected += opusSilenceSamples
	}
}

func (s *oggSink) Gap()          {}
func (s *oggSink) Started() bool { return s.started }
func (s *oggSink) Close() error  { return s.w.Close() }

// videoCodec is what an ivfSink needs to know about a codec.
type videoCodec struct {
	fourcc     string
	isKeyFrame func([]byte) bool
	// dimensions reads a key frame's size; ok=false means "not a usable start".
	dimensions func([]byte) (width, height uint16, ok bool)
}

var (
	vp8Codec  = videoCodec{fourcc: "VP80", isKeyFrame: vp8IsKeyFrame, dimensions: vp8Dimensions}
	h264Codec = videoCodec{fourcc: "H264", isKeyFrame: h264IsKeyFrame, dimensions: h264Dimensions}
)

// ivfSink writes VP8 or H.264 (Annex B) into IVF with a 90 kHz time base, so timestamps keep their
// gaps. ffmpeg reads both. It starts at, and after a gap resumes from, a key frame.
type ivfSink struct {
	codec    videoCodec
	f        *os.File
	ts       tsUnwrapper
	frames   uint32
	waitKey  bool
	started  bool
	lastPTS  int64
	hasFrame bool
}

const ivfHeaderSize = 32

func newIVFSink(path string, codec videoCodec) (*ivfSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &ivfSink{codec: codec, f: f, waitKey: true}, nil
}

func (s *ivfSink) writeHeader(width, height uint16) error {
	h := make([]byte, ivfHeaderSize)
	copy(h[0:], "DKIF")
	binary.LittleEndian.PutUint16(h[4:], 0) // version
	binary.LittleEndian.PutUint16(h[6:], ivfHeaderSize)
	copy(h[8:], s.codec.fourcc)
	binary.LittleEndian.PutUint16(h[12:], width)
	binary.LittleEndian.PutUint16(h[14:], height)
	binary.LittleEndian.PutUint32(h[16:], 90000) // time base denominator
	binary.LittleEndian.PutUint32(h[20:], 1)     // time base numerator
	// h[24:28] is patched on Close. libvpx calls it the frame count, but ffmpeg reads (and its own
	// IVF muxer writes) the duration in time base units, so that is what we store.
	_, err := s.f.WriteAt(h, 0)
	return err
}

func (s *ivfSink) WriteFrame(frame []byte, rtpTS uint32) error {
	if s.waitKey && !s.codec.isKeyFrame(frame) {
		return nil
	}
	if !s.started {
		width, height, ok := s.codec.dimensions(frame)
		if !ok {
			return nil
		}
		if err := s.writeHeader(width, height); err != nil {
			return err
		}
		if _, err := s.f.Seek(ivfHeaderSize, 0); err != nil {
			return err
		}
		s.started = true
	}
	s.waitKey = false
	pts := s.ts.unwrap(rtpTS)
	// Simulcast layers share the RTP clock, but keep IVF strictly increasing anyway.
	if s.hasFrame && pts <= s.lastPTS {
		pts = s.lastPTS + 1
	}
	s.lastPTS, s.hasFrame = pts, true
	fh := make([]byte, 12)
	binary.LittleEndian.PutUint32(fh[0:], uint32(len(frame)))
	binary.LittleEndian.PutUint64(fh[4:], uint64(pts))
	if _, err := s.f.Write(fh); err != nil {
		return err
	}
	if _, err := s.f.Write(frame); err != nil {
		return err
	}
	s.frames++
	return nil
}

func (s *ivfSink) Gap()          { s.waitKey = true }
func (s *ivfSink) Started() bool { return s.started }

func (s *ivfSink) Close() error {
	defer s.f.Close()
	if !s.started {
		return nil
	}
	duration := s.lastPTS
	if s.frames > 1 {
		duration += s.lastPTS / int64(s.frames-1) // plus one average frame
	}
	field := make([]byte, 4)
	binary.LittleEndian.PutUint32(field, uint32(min(duration, int64(^uint32(0)))))
	if _, err := s.f.WriteAt(field, 24); err != nil {
		return fmt.Errorf("patch IVF duration: %w", err)
	}
	return nil
}
