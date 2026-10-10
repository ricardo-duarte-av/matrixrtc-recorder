// Package recorder subscribes to a MatrixRTC call's LiveKit SFUs and writes every remote track,
// decrypted with the participants' per-participant keys, to its own file.
package recorder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/livekit/server-sdk-go/v2/e2ee"
	"github.com/livekit/server-sdk-go/v2/pkg/samplebuilder"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/rs/zerolog"
)

// Participant is who an SFU identity belongs to, as far as Matrix knows.
type Participant struct {
	UserID   string `json:"user_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

// TrackInfo describes one recorded file, for whoever mixes the tracks afterwards.
type TrackInfo struct {
	File        string      `json:"file,omitempty"`
	Identity    string      `json:"identity"`
	Participant Participant `json:"participant"`
	TrackSID    string      `json:"track_sid"`
	Kind        string      `json:"kind"`
	Source      string      `json:"source"`
	Codec       string      `json:"codec"`
	// FirstFrameAt is the wall-clock arrival of the first written frame: line tracks up by it.
	FirstFrameAt  *time.Time `json:"first_frame_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	Frames        int        `json:"frames"`
	Undecryptable int        `json:"undecryptable_frames"`
	Packets       int        `json:"packets"`
	Samples       int        `json:"samples"`
}

// Meta is written next to the tracks as meta.json.
type Meta struct {
	RoomID    string       `json:"room_id"`
	Encrypted bool         `json:"encrypted"`
	StartedAt time.Time    `json:"started_at"`
	EndedAt   *time.Time   `json:"ended_at,omitempty"`
	Tracks    []*TrackInfo `json:"tracks"`
}

// Session is one recording: connections to every SFU the call's members publish on.
type Session struct {
	Dir       string
	log       zerolog.Logger
	encrypted bool
	lookup    func(identity string) Participant

	mu     sync.Mutex
	conns  map[string]*lksdk.Room
	keys   map[string]*e2ee.ExternalKeyProvider
	meta   Meta
	closed bool
	wg     sync.WaitGroup
}

// maxLate is how many packets the sample builder waits for a missing one before giving up on it.
const maxLate = 200

// pliInterval rate-limits key frame requests while a video track waits for a decryptable key frame.
const pliInterval = time.Second

func NewSession(dir, roomID string, encrypted bool, log zerolog.Logger, lookup func(identity string) Participant) (*Session, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	return &Session{
		Dir:       dir,
		log:       log.With().Str("component", "recorder").Logger(),
		encrypted: encrypted,
		lookup:    lookup,
		conns:     make(map[string]*lksdk.Room),
		keys:      make(map[string]*e2ee.ExternalKeyProvider),
		meta:      Meta{RoomID: roomID, Encrypted: encrypted, StartedAt: time.Now()},
	}, nil
}

// Connected tells whether we're already connected to the SFU with this WebSocket URL.
func (s *Session) Connected(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[url]
	return ok
}

// Connect joins an SFU (subscribe only) and starts recording whatever is published there.
func (s *Session) Connect(url, jwt string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("session closed")
	}
	if _, ok := s.conns[url]; ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var room *lksdk.Room
	cb := lksdk.NewRoomCallback()
	cb.OnTrackSubscribed = func(track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.record(room, track, pub, rp)
		}()
	}
	cb.OnDisconnectedWithReason = func(reason lksdk.DisconnectionReason) {
		s.log.Info().Str("sfu", url).Str("reason", string(reason)).Msg("Disconnected from SFU")
		s.mu.Lock()
		if s.conns[url] == room {
			delete(s.conns, url)
		}
		s.mu.Unlock()
	}
	room = lksdk.NewRoom(cb)
	if err := room.JoinWithToken(url, jwt, lksdk.WithAutoSubscribe(true)); err != nil {
		return fmt.Errorf("connect to SFU %s: %w", url, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		go room.Disconnect()
		return fmt.Errorf("session closed")
	}
	s.conns[url] = room
	s.log.Info().Str("sfu", url).Str("livekit_room", room.Name()).Msg("Connected to SFU")
	return nil
}

// SetKey installs a participant's media key: raw Matrix key material, run through LiveKit's HKDF.
func (s *Session) SetKey(identity string, index int, raw []byte) {
	derived, err := lksdk.DeriveKeyFromBytes(raw)
	if err != nil {
		s.log.Err(err).Str("identity", identity).Msg("Failed to derive media key")
		return
	}
	if err = s.provider(identity).SetRawKey(derived, uint32(index)); err != nil {
		s.log.Err(err).Str("identity", identity).Msg("Failed to set media key")
		return
	}
	s.log.Debug().Str("identity", identity).Int("index", index).Msg("Got media key")
}

func (s *Session) provider(identity string) *e2ee.ExternalKeyProvider {
	s.mu.Lock()
	defer s.mu.Unlock()
	kp, ok := s.keys[identity]
	if !ok {
		kp = e2ee.NewExternalKeyProvider()
		s.keys[identity] = kp
	}
	return kp
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (s *Session) record(room *lksdk.Room, track *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	identity := rp.Identity()
	mime := strings.ToLower(track.Codec().MimeType)
	log := s.log.With().Str("identity", identity).Str("track", pub.SID()).Str("codec", mime).Logger()

	var (
		ext          string
		decryptFn    e2ee.DecryptFunc
		depacketizer rtp.Depacketizer
		video        *videoCodec
	)
	switch mime {
	case strings.ToLower(webrtc.MimeTypeOpus):
		ext, decryptFn, depacketizer = "ogg", lksdk.DecryptGCMAudioSampleCustomCipher, &codecs.OpusPacket{}
	case strings.ToLower(webrtc.MimeTypeVP8):
		ext, decryptFn, depacketizer, video = "ivf", decryptVP8, &codecs.VP8Packet{}, &vp8Codec
	case strings.ToLower(webrtc.MimeTypeH264):
		ext, decryptFn, depacketizer, video = "h264.ivf", e2ee.DecryptGCMH264SampleCustomCipher, &codecs.H264Packet{}, &h264Codec
	default:
		log.Warn().Msg("Codec not supported yet, not recording this track")
		_ = pub.SetSubscribed(false)
		return
	}

	participant := s.lookup(identity)
	who := participant.UserID
	if who == "" {
		who = identity
	}
	name := fmt.Sprintf("%s_%s_%s.%s", unsafeName.ReplaceAllString(strings.TrimPrefix(who, "@"), "_"),
		unsafeName.ReplaceAllString(participant.DeviceID, "_"), pub.SID(), ext)
	path := filepath.Join(s.Dir, name)

	var sink frameSink
	var err error
	if video != nil {
		sink, err = newIVFSink(path, *video)
	} else {
		sink, err = newOggSink(path)
	}
	if err != nil {
		log.Err(err).Msg("Failed to create track file")
		return
	}
	info := &TrackInfo{
		File:        name,
		Identity:    identity,
		Participant: participant,
		TrackSID:    pub.SID(),
		Kind:        string(pub.Kind()),
		Source:      pub.Source().String(),
		Codec:       mime,
	}
	s.mu.Lock()
	s.meta.Tracks = append(s.meta.Tracks, info)
	s.mu.Unlock()
	log.Info().Str("file", name).Msg("Recording track")

	if video != nil {
		_ = pub.SetVideoQuality(livekit.VideoQuality_HIGH)
	}

	var decryptor *e2ee.GCMFrameDecryptor
	if s.encrypted {
		decryptor = e2ee.NewGCMFrameDecryptor(s.provider(identity), decryptFn, room.SifTrailer())
	}
	sb := samplebuilder.New(maxLate, depacketizer, track.Codec().ClockRate)

	var lastPLI time.Time
	requestKeyFrame := func() {
		if video != nil && time.Since(lastPLI) >= pliInterval {
			lastPLI = time.Now()
			rp.WritePLI(track.SSRC())
		}
	}
	requestKeyFrame()

	// handle decrypts and writes one whole frame (for video: one access unit).
	handled := 0
	lastDecryptErr := ""
	handle := func(frame []byte, rtpTS uint32) {
		handled++
		if video != nil && handled <= 3 {
			log.Debug().Int("bytes", len(frame)).Bool("key_frame", video.isKeyFrame(frame)).
				Hex("head", frame[:min(len(frame), 16)]).Msg("Video access unit")
		}
		if decryptor != nil && len(frame) > 0 {
			var err error
			raw := frame
			frame, err = decryptor.DecryptFrame(frame)
			if err == nil && lastDecryptErr != "" {
				lastDecryptErr = ""
				log.Info().Int("key_index", int(raw[len(raw)-1])).Msg("Decrypting again")
			}
			if err != nil {
				// Log whenever the reason changes (e.g. missing key 0, then a bad key 1), not just once.
				if msg := err.Error(); msg != lastDecryptErr || info.Undecryptable%500 == 0 {
					lastDecryptErr = msg
					log.Warn().Err(err).Int("key_index", int(raw[len(raw)-1])).
						Int("undecryptable", info.Undecryptable+1).Msg("Can't decrypt frame")
				}
				info.Undecryptable++
				sink.Gap()
				requestKeyFrame()
				return
			}
			if frame == nil { // server-injected frame
				return
			}
		}
		if err := sink.WriteFrame(frame, rtpTS); err != nil {
			log.Err(err).Msg("Failed to write frame")
			return
		}
		if !sink.Started() {
			requestKeyFrame()
			return
		}
		if info.FirstFrameAt == nil {
			now := time.Now()
			info.FirstFrameAt = &now
			log.Info().Msg("First frame written")
		}
		info.Frames++
	}

	units := &unitAssembler{emit: handle}

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			break
		}
		info.Packets++
		sb.Push(pkt)
		if video != nil && !sink.Started() && info.Packets%500 == 0 {
			log.Debug().Int("packets", info.Packets).Int("samples", info.Samples).Msg("Video track still waiting for a key frame")
		}
		for {
			// Pop leaves PacketTimestamp unset; the RTP timestamp is what keeps DTX gaps and video timing.
			sample, rtpTS := sb.PopWithTimestamp()
			if sample == nil {
				break
			}
			info.Samples++
			if sample.PrevDroppedPackets > 0 {
				sink.Gap()
			}
			if video == nil {
				handle(sample.Data, rtpTS)
			} else {
				units.push(sample.Data, rtpTS, sample.PrevDroppedPackets > 0)
			}
		}
	}
	units.flush()

	if padder, ok := sink.(interface{ PadTo(time.Time) }); ok {
		// A muted track sends nothing: without this its file would stop at the mute.
		padder.PadTo(time.Now())
	}
	if err = sink.Close(); err != nil {
		log.Err(err).Msg("Failed to close track file")
	}
	if !sink.Started() {
		// Muted throughout (or never decryptable): an empty file would only look broken.
		_ = os.Remove(path)
		s.mu.Lock()
		info.File = ""
		s.mu.Unlock()
	}
	ended := time.Now()
	s.mu.Lock()
	info.EndedAt = &ended
	s.mu.Unlock()
	log.Info().Int("frames", info.Frames).Int("undecryptable", info.Undecryptable).
		Int("packets", info.Packets).Int("samples", info.Samples).Msg("Track ended")
}

// Close disconnects from every SFU, finishes the track files and writes meta.json.
func (s *Session) Close() Meta {
	s.mu.Lock()
	s.closed = true
	conns := s.conns
	s.conns = map[string]*lksdk.Room{}
	s.mu.Unlock()
	for _, room := range conns {
		room.Disconnect()
	}

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		s.log.Warn().Msg("Timed out waiting for tracks to finish")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ended := time.Now()
	s.meta.EndedAt = &ended
	if raw, err := json.MarshalIndent(&s.meta, "", "  "); err != nil {
		s.log.Err(err).Msg("Failed to encode meta.json")
	} else if err = os.WriteFile(filepath.Join(s.Dir, "meta.json"), raw, 0o640); err != nil {
		s.log.Err(err).Msg("Failed to write meta.json")
	}
	return s.meta
}

// unitAssembler puts video access units back together. The sample builder starts a new sample at
// every partition head, so it splits an H.264 access unit sent as SPS/PPS (STAP-A) then the IDR
// slice (FU-A) in two. LiveKit encrypts the whole access unit as one frame, so pieces with the same
// RTP timestamp are joined before decryption.
type unitAssembler struct {
	emit func(frame []byte, rtpTS uint32)
	unit []byte
	ts   uint32
}

func (a *unitAssembler) push(piece []byte, rtpTS uint32, lost bool) {
	if len(a.unit) > 0 && rtpTS != a.ts {
		a.flush()
	}
	if lost {
		a.unit = nil // what we hold is missing pieces
	}
	a.unit = append(a.unit, piece...)
	a.ts = rtpTS
}

func (a *unitAssembler) flush() {
	if len(a.unit) > 0 {
		a.emit(a.unit, a.ts)
	}
	a.unit = nil
}
