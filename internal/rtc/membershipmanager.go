package rtc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Element Call's defaults: gone 18 s after we stop answering.
const (
	leaveDelay       = 18 * time.Second
	leaveRestart     = 4 * time.Second
	refreshHeadroom  = 5 * time.Second
	membershipIntent = IntentAudio
)

// MembershipManager keeps our own call membership in the room for as long as we're in the call,
// in either format, the way matrix-js-sdk's MembershipManager does:
//
//  1. schedule our leave as a delayed event (MSC4140), so a crash or lost network still leaves;
//  2. send the membership (a state event, or an MSC4354 sticky event);
//  3. keep restarting the delayed leave; reschedule it if the server forgot it;
//  4. re-send the membership before it expires (legacy `expires`, sticky duration);
//  5. on leave, fire the delayed leave now (or send a leave ourselves if that fails).
type MembershipManager struct {
	cli        *mautrix.Client
	log        zerolog.Logger
	roomID     id.RoomID
	format     Format
	memberID   string
	stateKey   string
	transports []Transport

	mu          sync.Mutex
	delayID     id.DelayID
	joined      bool
	createdTS   *time.Time
	expiryIters int
	cancel      context.CancelFunc
	done        sync.WaitGroup
}

func NewMembershipManager(cli *mautrix.Client, log zerolog.Logger, roomID id.RoomID, roomVersion string, format Format, memberID string, transports []Transport) *MembershipManager {
	return &MembershipManager{
		cli:         cli,
		log:         log.With().Str("component", "membership").Logger(),
		roomID:      roomID,
		format:      format,
		memberID:    memberID,
		stateKey:    LegacyStateKey(cli.UserID, cli.DeviceID, roomVersion),
		transports:  transports,
		expiryIters: 1,
	}
}

// Join schedules the delayed leave, then sends the membership. Returns our membership event id.
func (m *MembershipManager) Join(ctx context.Context) (id.EventID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.scheduleDelayedLeave(ctx); err != nil {
		return "", err
	}
	evtID, err := m.sendMembership(ctx)
	if err != nil {
		m.cancelDelayedLeave(ctx)
		return "", err
	}
	m.joined = true
	now := time.Now()
	m.createdTS = &now
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.cancel = cancel
	m.done.Add(2)
	go m.heartbeat(loopCtx)
	go m.refresher(loopCtx)
	return evtID, nil
}

// Leave sends the delayed leave now, so the server-side copy and ours can't both apply.
func (m *MembershipManager) Leave(ctx context.Context) {
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.done.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.joined {
		m.cancelDelayedLeave(ctx)
		return
	}
	m.joined = false
	sent := false
	if m.delayID != "" {
		_, err := m.cli.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{DelayID: m.delayID, Action: event.DelayActionSend})
		sent = err == nil
		if err != nil {
			m.log.Warn().Err(err).Msg("Failed to send delayed leave, sending a leave instead")
		}
	}
	if !sent {
		if err := m.sendLeave(ctx); err != nil {
			m.log.Err(err).Msg("Failed to send leave")
		}
	}
	m.delayID = ""
}

func (m *MembershipManager) cancelDelayedLeave(ctx context.Context) {
	if m.delayID == "" {
		return
	}
	_, err := m.cli.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{DelayID: m.delayID, Action: event.DelayActionCancel})
	if err != nil {
		m.log.Warn().Err(err).Msg("Failed to cancel delayed leave")
	}
	m.delayID = ""
}

func (m *MembershipManager) scheduleDelayedLeave(ctx context.Context) error {
	var resp *mautrix.RespSendEvent
	var err error
	switch m.format {
	case FormatLegacy:
		resp, err = m.cli.SendStateEvent(ctx, m.roomID, LegacyMemberEventType, m.stateKey, map[string]any{},
			mautrix.ReqSendEvent{UnstableDelay: leaveDelay})
	case FormatSticky:
		resp, err = m.cli.SendMessageEvent(ctx, m.roomID, StickyMemberEventType, StickyLeaveContent(m.memberID),
			mautrix.ReqSendEvent{UnstableDelay: leaveDelay, UnstableStickyDuration: StickyDuration, DontEncrypt: true})
	}
	if err != nil {
		return err
	}
	m.delayID = resp.UnstableDelayID
	return nil
}

func (m *MembershipManager) sendMembership(ctx context.Context) (id.EventID, error) {
	var resp *mautrix.RespSendEvent
	var err error
	switch m.format {
	case FormatLegacy:
		content := LegacyContent(m.cli.UserID, m.cli.DeviceID, membershipIntent, m.transports,
			DefaultLegacyExpiry*time.Duration(m.expiryIters), m.createdTS)
		resp, err = m.cli.SendStateEvent(ctx, m.roomID, LegacyMemberEventType, m.stateKey, content)
	case FormatSticky:
		content := StickyContent(m.cli.UserID, m.cli.DeviceID, m.memberID, membershipIntent, m.transports)
		// Unencrypted, as gomuks sends them: clients read memberships before they have room keys.
		resp, err = m.cli.SendMessageEvent(ctx, m.roomID, StickyMemberEventType, content,
			mautrix.ReqSendEvent{UnstableStickyDuration: StickyDuration, DontEncrypt: true})
	}
	if err != nil {
		return "", err
	}
	return resp.EventID, nil
}

func (m *MembershipManager) sendLeave(ctx context.Context) error {
	var err error
	switch m.format {
	case FormatLegacy:
		_, err = m.cli.SendStateEvent(ctx, m.roomID, LegacyMemberEventType, m.stateKey, map[string]any{})
	case FormatSticky:
		_, err = m.cli.SendMessageEvent(ctx, m.roomID, StickyMemberEventType, StickyLeaveContent(m.memberID),
			mautrix.ReqSendEvent{UnstableStickyDuration: StickyDuration, DontEncrypt: true})
	}
	return err
}

// heartbeat pushes the delayed leave back; reschedules it when the server no longer knows it.
// A network error needs nothing: the next beat tries again, well within the delay.
func (m *MembershipManager) heartbeat(ctx context.Context) {
	defer m.done.Done()
	ticker := time.NewTicker(leaveRestart)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		id := m.delayID
		m.mu.Unlock()
		forgotten := id == ""
		if !forgotten {
			_, err := m.cli.UpdateDelayedEvent(ctx, &mautrix.ReqUpdateDelayedEvent{DelayID: id, Action: event.DelayActionRestart})
			forgotten = errors.Is(err, mautrix.MNotFound)
			if err != nil && !forgotten && ctx.Err() == nil {
				m.log.Debug().Err(err).Msg("Failed to restart delayed leave")
			}
		}
		if forgotten {
			m.mu.Lock()
			if m.joined && ctx.Err() == nil {
				if err := m.scheduleDelayedLeave(ctx); err != nil {
					m.log.Warn().Err(err).Msg("Failed to reschedule delayed leave")
				}
			}
			m.mu.Unlock()
		}
	}
}

// refresher re-sends the membership shortly before it would lapse.
func (m *MembershipManager) refresher(ctx context.Context) {
	defer m.done.Done()
	lifetime := StickyDuration
	if m.format == FormatLegacy {
		lifetime = DefaultLegacyExpiry
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(lifetime - refreshHeadroom):
		}
		m.mu.Lock()
		if m.joined {
			if m.format == FormatLegacy {
				m.expiryIters++
			}
			if _, err := m.sendMembership(ctx); err != nil {
				m.log.Warn().Err(err).Msg("Failed to refresh membership")
			}
		}
		m.mu.Unlock()
	}
}
