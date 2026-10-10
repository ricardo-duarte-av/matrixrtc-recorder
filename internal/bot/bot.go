// Package bot is the Matrix side of the recorder: it joins rooms it's invited to, keeps track of
// who is in each room's call, takes `!record` commands from moderators, and joins calls to record them.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/ricardo-duarte-av/matrixrtc-recorder/internal/config"
	"github.com/ricardo-duarte-av/matrixrtc-recorder/internal/rtc"
)

type Bot struct {
	cfg     *config.Config
	log     zerolog.Logger
	cli     *mautrix.Client
	crypto  *cryptohelper.CryptoHelper
	started time.Time
	// ctx outlives single sync handlers: recordings run on it.
	ctx context.Context

	mu    sync.Mutex
	rooms map[id.RoomID]*rtc.Members
	calls map[id.RoomID]*call
}

// memorySyncStore keeps the sync token in memory only, so every start is an initial sync: that is
// how we learn the sticky memberships of calls already in progress. The type is distinct from
// *mautrix.MemorySyncStore so the crypto helper doesn't swap it for its persistent store.
type memorySyncStore struct{ *mautrix.MemorySyncStore }

func New(ctx context.Context, cfg *config.Config, log zerolog.Logger) (*Bot, error) {
	cli, err := mautrix.NewClient(cfg.Homeserver, cfg.UserID, "")
	if err != nil {
		return nil, err
	}
	cli.Log = log.With().Str("component", "matrix").Logger()
	cli.Store = memorySyncStore{mautrix.NewMemorySyncStore()}

	b := &Bot{
		cfg:     cfg,
		log:     log,
		cli:     cli,
		started: time.Now(),
		ctx:     ctx,
		rooms:   make(map[id.RoomID]*rtc.Members),
		calls:   make(map[id.RoomID]*call),
	}

	syncer := cli.Syncer.(*mautrix.DefaultSyncer)
	syncer.FilterJSON = &mautrix.Filter{Room: &mautrix.RoomFilter{Timeline: &mautrix.FilterPart{Limit: 20}}}
	// Must come before the crypto helper registers its own to-device handling: see interceptToDevice.
	syncer.OnSync(b.interceptToDevice)
	syncer.OnEventType(event.StateMember, b.onMember)
	syncer.OnEventType(event.EventMessage, b.onMessage)
	syncer.OnEvent(b.onAnyEvent)

	helper, err := cryptohelper.NewCryptoHelper(cli, []byte(cfg.PickleKey), cfg.Database)
	if err != nil {
		return nil, err
	}
	helper.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: string(cfg.UserID)},
		Password:                 cfg.Password,
		InitialDeviceDisplayName: cfg.DeviceName,
	}
	if err = helper.Init(ctx); err != nil {
		return nil, fmt.Errorf("init encryption: %w", err)
	}
	cli.Crypto = helper
	b.crypto = helper
	if _, err = cli.Versions(ctx); err != nil {
		return nil, fmt.Errorf("fetch server versions: %w", err)
	}
	log.Info().Stringer("user_id", cli.UserID).Stringer("device_id", cli.DeviceID).Msg("Logged in")
	return b, nil
}

// Run syncs until ctx is done, then stops every recording.
func (b *Bot) Run(ctx context.Context) error {
	go b.tick(ctx)
	err := b.cli.SyncWithContext(ctx)
	b.stopAll("the recorder is shutting down")
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (b *Bot) members(roomID id.RoomID) *rtc.Members {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.rooms[roomID]
	if !ok {
		m = rtc.NewMembers()
		b.rooms[roomID] = m
	}
	return m
}

// interceptToDevice pulls Olm-encrypted to-device events out of the sync before the crypto helper
// sees them, and decrypts them itself. mautrix's OlmMachine drops decrypted to-device events of
// types it doesn't know — like call encryption keys — but HandleEncryptedEvent hands them back.
func (b *Bot) interceptToDevice(ctx context.Context, resp *mautrix.RespSync, _ string) bool {
	mach := b.crypto.Machine()
	kept := resp.ToDevice.Events[:0]
	for _, evt := range resp.ToDevice.Events {
		if evt.Type.Type != event.ToDeviceEncrypted.Type {
			kept = append(kept, evt)
			continue
		}
		evt.Type.Class = event.ToDeviceEventType
		if err := evt.Content.ParseRaw(evt.Type); err != nil {
			b.log.Warn().Err(err).Msg("Failed to parse encrypted to-device event")
			continue
		}
		decrypted := mach.HandleEncryptedEvent(ctx, evt)
		if decrypted == nil || decrypted.Type.Type != rtc.EncryptionKeysType {
			continue
		}
		b.onMediaKey(decrypted.Sender, decrypted.SenderDevice, decrypted.Content.VeryRaw)
	}
	resp.ToDevice.Events = kept
	return true
}

func (b *Bot) onMediaKey(sender id.UserID, senderDevice *id.Device, raw json.RawMessage) {
	mk := rtc.ParseMediaKey(sender, raw)
	if mk == nil {
		b.log.Debug().Stringer("sender", sender).Msg("Ignoring malformed media key")
		return
	}
	// A key is only good for the device that sent it, whatever the payload claims.
	if senderDevice != nil && senderDevice.DeviceID != mk.DeviceID {
		b.log.Warn().Stringer("sender", sender).Stringer("device", senderDevice.DeviceID).
			Stringer("claimed_device", mk.DeviceID).Msg("Ignoring media key claiming another device")
		return
	}
	b.mu.Lock()
	c := b.calls[mk.RoomID]
	b.mu.Unlock()
	if c == nil {
		return
	}
	b.log.Debug().Stringer("room_id", mk.RoomID).Stringer("sender", sender).Stringer("device", mk.DeviceID).
		Int("index", mk.Index).Msg("Received media key")
	c.keys.OnKey(mk)
}

func (b *Bot) onMember(ctx context.Context, evt *event.Event) {
	if evt.GetStateKey() != b.cli.UserID.String() {
		return
	}
	if evt.Content.AsMember().Membership == event.MembershipInvite {
		go func() {
			if _, err := b.cli.JoinRoomByID(b.ctx, evt.RoomID); err != nil {
				b.log.Err(err).Stringer("room_id", evt.RoomID).Msg("Failed to join room")
				return
			}
			b.log.Info().Stringer("room_id", evt.RoomID).Stringer("inviter", evt.Sender).Msg("Joined room")
		}()
	}
}

func (b *Bot) onAnyEvent(ctx context.Context, evt *event.Event) {
	if !rtc.IsMembershipEvent(evt) || evt.RoomID == "" {
		return
	}
	if b.members(evt.RoomID).Apply(evt, time.Now()) {
		b.membersChanged(evt.RoomID)
	}
}

// tick notices memberships that lapse without a leave event (and anything else we missed).
func (b *Bot) tick(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		b.mu.Lock()
		rooms := make([]id.RoomID, 0, len(b.calls))
		for roomID := range b.calls {
			rooms = append(rooms, roomID)
		}
		b.mu.Unlock()
		for _, roomID := range rooms {
			b.membersChanged(roomID)
		}
	}
}

func isInitialSync(ctx context.Context) bool {
	since, _ := ctx.Value(mautrix.SyncTokenContextKey).(string)
	return since == ""
}

func (b *Bot) onMessage(ctx context.Context, evt *event.Event) {
	// Commands in the history an initial sync replays were for an earlier run.
	if evt.Sender == b.cli.UserID || isInitialSync(ctx) || evt.Timestamp < b.started.Add(-time.Minute).UnixMilli() {
		return
	}
	content := evt.Content.AsMessage()
	if content.MsgType != event.MsgText {
		return
	}
	fields := strings.Fields(content.Body)
	if len(fields) == 0 || !strings.EqualFold(fields[0], b.cfg.CommandPrefix) {
		return
	}
	sub := "help"
	if len(fields) > 1 {
		sub = strings.ToLower(fields[1])
	}
	switch sub {
	case "start", "stop":
		if level, ok := b.allowed(ctx, evt.RoomID, evt.Sender); !ok {
			b.reply(evt.RoomID, fmt.Sprintf("Only members with power level %d or higher can %s recordings (yours is %d).",
				b.cfg.MinPowerLevel, sub, level))
			return
		}
		if sub == "start" {
			go b.startRecording(evt.RoomID, evt.Sender)
		} else {
			go b.stopRecording(evt.RoomID, fmt.Sprintf("stopped by %s", evt.Sender))
		}
	case "status":
		b.reply(evt.RoomID, b.status(evt.RoomID))
	default:
		p := b.cfg.CommandPrefix
		b.reply(evt.RoomID, fmt.Sprintf("`%s start` — join the ongoing call and record it\n\n"+
			"`%s stop` — stop recording\n\n`%s status` — show what's going on\n\n"+
			"Starting and stopping needs power level %d.", p, p, p, b.cfg.MinPowerLevel))
	}
}

func (b *Bot) allowed(ctx context.Context, roomID id.RoomID, userID id.UserID) (int, bool) {
	pl, err := b.cli.StateStore.GetPowerLevels(ctx, roomID)
	if err != nil || pl == nil {
		pl = &event.PowerLevelsEventContent{}
		if err = b.cli.StateEvent(ctx, roomID, event.StatePowerLevels, "", pl); err != nil {
			b.log.Err(err).Stringer("room_id", roomID).Msg("Failed to get power levels")
			return 0, false
		}
	}
	level := pl.GetUserLevel(userID)
	return level, level >= b.cfg.MinPowerLevel
}

func (b *Bot) reply(roomID id.RoomID, markdown string) {
	b.replyWith(b.ctx, roomID, markdown)
}

func (b *Bot) replyWith(ctx context.Context, roomID id.RoomID, markdown string) {
	content := format.RenderMarkdown(markdown, true, false)
	content.MsgType = event.MsgNotice
	if _, err := b.cli.SendMessageEvent(ctx, roomID, event.EventMessage, &content); err != nil {
		b.log.Err(err).Stringer("room_id", roomID).Msg("Failed to send message")
	}
}
