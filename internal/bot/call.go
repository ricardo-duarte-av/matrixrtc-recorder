package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrixrtc-recorder/internal/recorder"
	"github.com/ricardo-duarte-av/matrixrtc-recorder/internal/rtc"
)

// call is the recorder's presence in one room's call.
type call struct {
	roomID      id.RoomID
	format      rtc.Format
	memberID    string
	requestedBy id.UserID
	startedAt   time.Time
	keys        *rtc.KeyRing
	rec         *recorder.Session
	// membership is nil until we've joined; a call without it is still starting.
	membership *rtc.MembershipManager

	mu sync.Mutex
	// services are the lk-jwt-service URLs we're connected through (one per SFU).
	services map[string]bool
	// connectMu serialises connecting, so member updates racing each other join an SFU once.
	connectMu sync.Mutex
}

func (c *call) subject(b *Bot) rtc.TokenSubject {
	return rtc.TokenSubject{UserID: b.cli.UserID, DeviceID: b.cli.DeviceID, MemberID: c.memberID}
}

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (b *Bot) startRecording(roomID id.RoomID, requester id.UserID) {
	ctx := b.ctx
	log := b.log.With().Stringer("room_id", roomID).Logger()

	b.mu.Lock()
	if _, busy := b.calls[roomID]; busy {
		b.mu.Unlock()
		b.reply(roomID, "Already recording this call.")
		return
	}
	// Reserve the room so a second `start` can't race us; replaced below once set up.
	b.calls[roomID] = nil
	b.mu.Unlock()
	fail := func(msg string, err error) {
		b.mu.Lock()
		if c := b.calls[roomID]; c == nil {
			delete(b.calls, roomID)
		}
		b.mu.Unlock()
		if err != nil {
			log.Err(err).Msg(msg)
			msg = fmt.Sprintf("%s: %v", msg, err)
		}
		b.reply(roomID, msg)
	}

	others := rtc.Others(b.members(roomID).Active(time.Now()), b.cli.UserID, b.cli.DeviceID)
	if len(others) == 0 {
		fail("There's no call going on in this room.", nil)
		return
	}

	// Follow the call: whatever its first member uses, if the room lets us send it.
	pl, _ := b.cli.StateStore.GetPowerLevels(ctx, roomID)
	callFormat := permittedFormat(others[0].Format, pl, b.cli.UserID)
	memberID := rtc.LegacyIdentity(b.cli.UserID, b.cli.DeviceID)
	if callFormat == rtc.FormatSticky {
		memberID = uuid.NewString()
	}

	transport, err := b.ownTransport(ctx, others)
	if err != nil {
		fail("Can't find a call service to connect to", err)
		return
	}

	encrypted, err := b.cli.StateStore.IsEncrypted(ctx, roomID)
	if err != nil {
		fail("Can't tell whether the room is encrypted", err)
		return
	}
	dir := filepath.Join(b.cfg.RecordingsDir, unsafePath.ReplaceAllString(string(roomID), "_"),
		time.Now().UTC().Format("20060102T150405Z"))
	rec, err := recorder.NewSession(dir, string(roomID), encrypted, log, b.lookup(roomID))
	if err != nil {
		fail("Can't create the recording directory", err)
		return
	}

	c := &call{
		roomID:      roomID,
		format:      callFormat,
		memberID:    memberID,
		requestedBy: requester,
		startedAt:   time.Now(),
		rec:         rec,
		keys:        rtc.NewKeyRing(roomID, rec.SetKey),
		services:    map[string]bool{},
	}
	c.keys.OnMembers(others)
	// Visible to onMediaKey from here on: keys start arriving as soon as our membership is out.
	b.mu.Lock()
	b.calls[roomID] = c
	b.mu.Unlock()
	abort := func(msg string, err error) {
		b.mu.Lock()
		delete(b.calls, roomID)
		b.mu.Unlock()
		rec.Close()
		fail(msg, err)
	}

	if err = b.connect(ctx, c, transport); err != nil {
		abort("Can't connect to the call's media server", err)
		return
	}

	var roomVersion string
	if callFormat == rtc.FormatLegacy {
		var create event.CreateEventContent
		if err = b.cli.StateEvent(ctx, roomID, event.StateCreate, "", &create); err == nil {
			roomVersion = string(create.RoomVersion)
		}
	}
	membership := rtc.NewMembershipManager(b.cli, log, roomID, roomVersion, callFormat, memberID, []rtc.Transport{transport})
	if _, err = membership.Join(ctx); err != nil {
		abort("Can't join the call", err)
		return
	}
	c.mu.Lock()
	c.membership = membership
	c.mu.Unlock()
	log.Info().Str("format", callFormat.String()).Str("member_id", memberID).Str("dir", dir).Msg("Recording started")

	b.connectOthers(c, others)
	note := ""
	if encrypted {
		note = " The call is end-to-end encrypted: the recorder only gets your media keys because it joined as a participant."
	}
	b.reply(roomID, fmt.Sprintf("🔴 **Recording started** at the request of %s. The recorder shows up as a participant in the call.%s",
		requester, note))
}

// permittedFormat is the membership kind the room's power levels allow: the wanted one if we may
// send it, else the other (Element Call and matrix-js-sdk read both), else the wanted one.
func permittedFormat(wanted rtc.Format, pl *event.PowerLevelsEventContent, userID id.UserID) rtc.Format {
	if pl == nil {
		return wanted
	}
	level := pl.GetUserLevel(userID)
	legacy := level >= pl.GetEventLevel(rtc.LegacyMemberEventType)
	sticky := level >= pl.GetEventLevel(rtc.StickyMemberEventType)
	switch {
	case wanted == rtc.FormatLegacy && !legacy && sticky:
		return rtc.FormatSticky
	case wanted == rtc.FormatSticky && !sticky && legacy:
		return rtc.FormatLegacy
	}
	return wanted
}

// ownTransport is the SFU we say we're on: the homeserver's, else the configured one, else the
// first member's.
func (b *Bot) ownTransport(ctx context.Context, others []*rtc.Membership) (rtc.Transport, error) {
	ts, err := rtc.Transports(ctx, b.cli)
	if err == nil && len(ts) > 0 {
		return ts[0], nil
	}
	if b.cfg.LivekitServiceURL != "" {
		return rtc.LivekitTransport(b.cfg.LivekitServiceURL), nil
	}
	for _, m := range others {
		if t := m.Transport(); t != nil && t.Type() == rtc.TransportLivekit {
			return t, nil
		}
	}
	if err == nil {
		err = fmt.Errorf("the homeserver advertises no LiveKit transport")
	}
	return nil, err
}

// connect gets a token from a transport's JWT service and joins that SFU, once per service.
func (b *Bot) connect(ctx context.Context, c *call, t rtc.Transport) error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	service := t.LivekitServiceURL()
	c.mu.Lock()
	done := c.services[service]
	c.mu.Unlock()
	if done {
		return nil
	}
	access, err := rtc.GetSFUAccess(ctx, b.cli, t, c.roomID, c.subject(b), c.format == rtc.FormatLegacy)
	if err != nil {
		return err
	}
	if err = c.rec.Connect(access.URL, access.JWT); err != nil {
		return err
	}
	c.mu.Lock()
	c.services[service] = true
	c.mu.Unlock()
	return nil
}

// connectOthers joins the SFUs other members publish on (multi-SFU calls: everyone their own).
func (b *Bot) connectOthers(c *call, others []*rtc.Membership) {
	for _, m := range others {
		t := m.Transport()
		if t == nil || t.Type() != rtc.TransportLivekit {
			continue
		}
		if err := b.connect(b.ctx, c, t); err != nil {
			b.log.Warn().Err(err).Stringer("room_id", c.roomID).Str("service", t.LivekitServiceURL()).
				Stringer("member", m.UserID).Msg("Can't connect to a member's SFU")
		}
	}
}

func (b *Bot) membersChanged(roomID id.RoomID) {
	b.mu.Lock()
	c := b.calls[roomID]
	b.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	joined := c.membership != nil
	c.mu.Unlock()
	others := rtc.Others(b.members(roomID).Active(time.Now()), b.cli.UserID, b.cli.DeviceID)
	c.keys.OnMembers(others)
	if !joined {
		return
	}
	if len(others) == 0 {
		go b.stopRecording(roomID, "everyone left the call")
		return
	}
	go b.connectOthers(c, others)
}

// lookup maps an SFU identity back to the Matrix user and device, for file names and meta.json.
func (b *Bot) lookup(roomID id.RoomID) func(string) recorder.Participant {
	return func(identity string) recorder.Participant {
		for _, m := range b.members(roomID).Active(time.Now()) {
			if m.RTCIdentity == identity {
				return recorder.Participant{UserID: string(m.UserID), DeviceID: string(m.DeviceID)}
			}
		}
		return recorder.Participant{}
	}
}

func (b *Bot) stopRecording(roomID id.RoomID, reason string) {
	b.mu.Lock()
	c, ok := b.calls[roomID]
	if !ok {
		b.mu.Unlock()
		b.reply(roomID, "Not recording here.")
		return
	}
	if c == nil {
		b.mu.Unlock()
		b.reply(roomID, "Still starting, try again in a moment.")
		return
	}
	c.mu.Lock()
	membership := c.membership
	c.mu.Unlock()
	if membership == nil {
		b.mu.Unlock()
		b.reply(roomID, "Still starting, try again in a moment.")
		return
	}
	delete(b.calls, roomID)
	b.mu.Unlock()

	// Leave even while shutting down: a lingering membership would keep showing us in the call.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.ctx), 15*time.Second)
	defer cancel()
	membership.Leave(ctx)
	meta := c.rec.Close()

	written := 0
	for _, t := range meta.Tracks {
		if t.Frames > 0 {
			written++
		}
	}
	b.log.Info().Stringer("room_id", roomID).Str("reason", reason).Int("tracks", written).Str("dir", c.rec.Dir).Msg("Recording stopped")
	msg := fmt.Sprintf("⏹ **Recording stopped** (%s) after %s. %d track(s) saved.",
		reason, time.Since(c.startedAt).Round(time.Second), written)
	b.replyWith(ctx, roomID, msg)
}

func (b *Bot) stopAll(reason string) {
	b.mu.Lock()
	rooms := make([]id.RoomID, 0, len(b.calls))
	for roomID, c := range b.calls {
		if c != nil {
			rooms = append(rooms, roomID)
		}
	}
	b.mu.Unlock()
	var wg sync.WaitGroup
	for _, roomID := range rooms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.stopRecording(roomID, reason)
		}()
	}
	wg.Wait()
}

func (b *Bot) status(roomID id.RoomID) string {
	b.mu.Lock()
	c, recording := b.calls[roomID]
	b.mu.Unlock()
	others := rtc.Others(b.members(roomID).Active(time.Now()), b.cli.UserID, b.cli.DeviceID)
	switch {
	case recording && c != nil:
		return fmt.Sprintf("🔴 Recording for %s (requested by %s), %d other participant(s) in the call.",
			time.Since(c.startedAt).Round(time.Second), c.requestedBy, len(others))
	case recording:
		return "Starting a recording…"
	case len(others) > 0:
		return fmt.Sprintf("Not recording. %d participant(s) in the call.", len(others))
	default:
		return "Not recording, and there's no call going on."
	}
}
