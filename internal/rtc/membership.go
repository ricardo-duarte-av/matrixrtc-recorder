// Package rtc speaks MatrixRTC (MSC4143) the way Element Call and matrix-js-sdk do today:
// call memberships in both encodings, per-participant media keys, and LiveKit SFU access.
//
// Ported from xmuks (core/protocol/rtc, core/call/signalling, core/call/media).
package rtc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Event types and constants of MatrixRTC calls as Element Call speaks them.
const (
	// LegacyMemberType is the state-event membership, one per device, left by sending `{}`.
	LegacyMemberType = "org.matrix.msc3401.call.member"
	// StickyMemberType is the MSC4354 sticky-event membership, keyed by a per-join member id.
	StickyMemberType = "org.matrix.msc4143.rtc.member"
	// EncryptionKeysType carries per-participant media keys, Olm-encrypted to-device.
	EncryptionKeysType = "io.element.call.encryption_keys"

	ApplicationCall  = "m.call"
	TransportLivekit = "livekit"
	IntentKey        = "m.call.intent"
	IntentAudio      = "audio"
	IntentVideo      = "video"

	// RoomSlotID is the room-wide call every client uses today.
	RoomSlotID = "m.call#ROOM"

	// DefaultLegacyExpiry is how long legacy memberships last after `created_ts` unless `expires` says otherwise.
	DefaultLegacyExpiry = 4 * time.Hour
	// StickyDuration is how long sticky memberships stay sticky; refreshed shortly before it runs out.
	StickyDuration = time.Hour
)

var (
	LegacyMemberEventType = event.Type{Type: LegacyMemberType, Class: event.StateEventType}
	StickyMemberEventType = event.Type{Type: StickyMemberType, Class: event.MessageEventType}
)

// Format is which of the two membership encodings an event (or our own join) uses.
type Format int

const (
	FormatLegacy Format = iota
	FormatSticky
)

func (f Format) String() string {
	if f == FormatSticky {
		return "sticky"
	}
	return "legacy"
}

// Transport is a MatrixRTC transport (MSC4143 focus), e.g. `{type: livekit, livekit_service_url}`.
// It is kept raw so we pass on fields we don't know about.
type Transport map[string]any

func (t Transport) Type() string {
	s, _ := t["type"].(string)
	return s
}

// LivekitServiceURL is the lk-jwt-service base URL, for `type == livekit`.
func (t Transport) LivekitServiceURL() string {
	s, _ := t["livekit_service_url"].(string)
	return s
}

func LivekitTransport(serviceURL string) Transport {
	return Transport{"type": TransportLivekit, "livekit_service_url": serviceURL}
}

// Membership is one device's presence in a room's call, parsed from either encoding.
type Membership struct {
	Format  Format
	RoomID  id.RoomID
	EventID id.EventID
	// Key identifies the membership slot later events replace: the state key (legacy) or sticky key.
	Key      string
	UserID   id.UserID
	DeviceID id.DeviceID
	MemberID string
	SlotID   string
	Intent   string
	// Transports are the SFUs this member publishes on (multi-SFU: everyone their own).
	Transports []Transport
	// CreatedTS is when this device joined (legacy `created_ts`, else the event's own timestamp).
	CreatedTS time.Time
	// ExpiresAt is when the membership stops counting, even without a leave.
	ExpiresAt time.Time
	// RTCIdentity is the participant identity on the SFU.
	RTCIdentity string
}

func (m *Membership) Transport() Transport {
	if len(m.Transports) == 0 {
		return nil
	}
	return m.Transports[0]
}

func (m *Membership) Expired(now time.Time) bool { return !now.Before(m.ExpiresAt) }

// session tells a new session of the same device apart from a refresh of the old one: a legacy
// rejoin keeps its member id but gets a new `created_ts`; a sticky one gets a new member id.
func (m *Membership) session() string {
	if m.Format == FormatLegacy {
		return fmt.Sprintf("%s/%d", m.MemberID, m.CreatedTS.UnixMilli())
	}
	return m.MemberID
}

type legacyContent struct {
	Application   string      `json:"application"`
	CallID        *string     `json:"call_id"`
	DeviceID      id.DeviceID `json:"device_id"`
	MembershipID  string      `json:"membershipID"`
	Intent        string      `json:"m.call.intent"`
	FocusActive   *Transport  `json:"focus_active"`
	FociPreferred []Transport `json:"foci_preferred"`
	CreatedTS     *int64      `json:"created_ts"`
	Expires       *int64      `json:"expires"`
}

type stickyContent struct {
	SlotID      string `json:"slot_id"`
	Application *struct {
		Type   string `json:"type"`
		Intent string `json:"m.call.intent"`
	} `json:"application"`
	Member *struct {
		UserID   id.UserID   `json:"user_id"`
		DeviceID id.DeviceID `json:"device_id"`
		ID       string      `json:"id"`
	} `json:"member"`
	Transports *struct {
		Published []Transport `json:"published"`
	} `json:"transports"`
	StickyKey         string `json:"sticky_key"`
	UnstableStickyKey string `json:"msc4354_sticky_key"`
}

func (c *stickyContent) key() string {
	if c.StickyKey != "" {
		return c.StickyKey
	}
	return c.UnstableStickyKey
}

// IsMembershipEvent tells whether evt is a call membership event of either encoding.
func IsMembershipEvent(evt *event.Event) bool {
	return evt.Type.Type == LegacyMemberType || evt.Type.Type == StickyMemberType
}

// KeyOf is the slot a membership event occupies (state key / sticky key), for leaves as well as joins.
func KeyOf(evt *event.Event) string {
	switch evt.Type.Type {
	case LegacyMemberType:
		if evt.StateKey != nil {
			return *evt.StateKey
		}
	case StickyMemberType:
		var c stickyContent
		if json.Unmarshal(evt.Content.VeryRaw, &c) == nil {
			return c.key()
		}
	}
	return ""
}

// Parse returns the membership an event carries, or nil when it's a leave, malformed, or not a membership.
func Parse(evt *event.Event, now time.Time) *Membership {
	switch evt.Type.Type {
	case LegacyMemberType:
		return parseLegacy(evt)
	case StickyMemberType:
		return parseSticky(evt, now)
	}
	return nil
}

func parseLegacy(evt *event.Event) *Membership {
	var c legacyContent
	if evt.StateKey == nil || json.Unmarshal(evt.Content.VeryRaw, &c) != nil {
		return nil
	}
	if c.DeviceID == "" || c.CallID == nil || c.Application == "" || c.FocusActive == nil || c.FocusActive.Type() == "" {
		return nil
	}
	created := time.UnixMilli(evt.Timestamp)
	if c.CreatedTS != nil {
		created = time.UnixMilli(*c.CreatedTS)
	}
	expires := DefaultLegacyExpiry
	if c.Expires != nil {
		expires = time.Duration(*c.Expires) * time.Millisecond
	}
	slot := *c.CallID
	if c.Application == ApplicationCall && slot == "" {
		slot = "ROOM"
	}
	memberID := c.MembershipID
	if memberID == "" {
		memberID = LegacyIdentity(evt.Sender, c.DeviceID)
	}
	return &Membership{
		Format:      FormatLegacy,
		RoomID:      evt.RoomID,
		EventID:     evt.ID,
		Key:         *evt.StateKey,
		UserID:      evt.Sender,
		DeviceID:    c.DeviceID,
		MemberID:    memberID,
		SlotID:      c.Application + "#" + slot,
		Intent:      c.Intent,
		Transports:  validTransports(c.FociPreferred),
		CreatedTS:   created,
		ExpiresAt:   created.Add(expires),
		RTCIdentity: LegacyIdentity(evt.Sender, c.DeviceID),
	}
}

func parseSticky(evt *event.Event, now time.Time) *Membership {
	var c stickyContent
	if json.Unmarshal(evt.Content.VeryRaw, &c) != nil || c.Application == nil || c.Member == nil || c.Transports == nil {
		return nil
	}
	if c.Application.Type == "" || containsHash(c.Application.Type) || !validSlot(c.SlotID, c.Application.Type) {
		return nil
	}
	// Only the member themself may say they're in the call.
	if c.Member.UserID != evt.Sender || c.Member.DeviceID == "" || c.Member.ID == "" || c.key() == "" {
		return nil
	}
	ts := time.UnixMilli(evt.Timestamp)
	// The server's remaining TTL is immune to clock skew; without it, count from the event's timestamp.
	expires := ts.Add(StickyDuration)
	if ttl := evt.Unsigned.StickyDurationTTL.Duration; ttl > 0 {
		expires = now.Add(ttl)
	} else if d := evt.Sticky.GetDuration(); d > 0 {
		expires = ts.Add(d)
	}
	return &Membership{
		Format:      FormatSticky,
		RoomID:      evt.RoomID,
		EventID:     evt.ID,
		Key:         c.key(),
		UserID:      c.Member.UserID,
		DeviceID:    c.Member.DeviceID,
		MemberID:    c.Member.ID,
		SlotID:      c.SlotID,
		Intent:      c.Application.Intent,
		Transports:  validTransports(c.Transports.Published),
		CreatedTS:   ts,
		ExpiresAt:   expires,
		RTCIdentity: StickyIdentity(c.Member.UserID, c.Member.DeviceID, c.Member.ID),
	}
}

func containsHash(s string) bool {
	for _, r := range s {
		if r == '#' {
			return true
		}
	}
	return false
}

func validSlot(slot, appType string) bool {
	if len(slot) <= len(appType)+1 || slot[:len(appType)+1] != appType+"#" {
		return false
	}
	return !containsHash(slot[len(appType)+1:])
}

func validTransports(ts []Transport) []Transport {
	out := make([]Transport, 0, len(ts))
	for _, t := range ts {
		if t.Type() != "" {
			out = append(out, t)
		}
	}
	return out
}

// LegacyIdentity is the SFU identity of a legacy member: what lk-jwt-service's `/sfu/get` assigns.
func LegacyIdentity(userID id.UserID, deviceID id.DeviceID) string {
	return fmt.Sprintf("%s:%s", userID, deviceID)
}

// StickyIdentity is the SFU identity of a sticky member: unpadded base64 of SHA-256 over
// the JSON array `["user","device","member"]`.
func StickyIdentity(userID id.UserID, deviceID id.DeviceID, memberID string) string {
	raw, _ := json.Marshal([]string{string(userID), string(deviceID), memberID})
	sum := sha256.Sum256(raw)
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

var ownedStateVersions = regexp.MustCompile(`^org\.matrix\.msc(3757|3779)\b`)

// LegacyStateKey is the state key of our own legacy membership (MSC3757/3779 rooms drop the `_`).
func LegacyStateKey(userID id.UserID, deviceID id.DeviceID, roomVersion string) string {
	key := fmt.Sprintf("%s_%s_%s", userID, deviceID, ApplicationCall)
	if ownedStateVersions.MatchString(roomVersion) {
		return key
	}
	return "_" + key
}

// LegacyContent is our own legacy membership, as matrix-js-sdk's `makeMyMembership` builds it.
func LegacyContent(userID id.UserID, deviceID id.DeviceID, intent string, transports []Transport, expires time.Duration, createdTS *time.Time) map[string]any {
	c := map[string]any{
		"application":  ApplicationCall,
		"call_id":      "",
		"scope":        "m.room",
		"device_id":    deviceID,
		"membershipID": LegacyIdentity(userID, deviceID),
		"expires":      expires.Milliseconds(),
		IntentKey:      intent,
		"focus_active": map[string]any{
			"type":            TransportLivekit,
			"focus_selection": "multi_sfu",
		},
		"foci_preferred": transports,
	}
	if createdTS != nil {
		c["created_ts"] = createdTS.UnixMilli()
	}
	return c
}

// StickyContent is our own sticky membership (`StickyEventMembershipManager.makeMyMembership`).
func StickyContent(userID id.UserID, deviceID id.DeviceID, memberID, intent string, transports []Transport) map[string]any {
	return map[string]any{
		"slot_id": RoomSlotID,
		"application": map[string]any{
			"type":    ApplicationCall,
			IntentKey: intent,
		},
		"transports": map[string]any{
			"published":     transports,
			"can_subscribe": []string{TransportLivekit},
		},
		"member": map[string]any{
			"device_id": deviceID,
			"user_id":   userID,
			"id":        memberID,
		},
		"versions":           []string{},
		"msc4354_sticky_key": memberID,
	}
}

// StickyLeaveContent is what a sticky leave (and the delayed leave scheduled at join) carries.
func StickyLeaveContent(memberID string) map[string]any {
	return map[string]any{
		"slot_id":            RoomSlotID,
		"msc4354_sticky_key": memberID,
	}
}

// rawContent lets mautrix "parse" MatrixRTC event types it doesn't know. We read their raw JSON
// ourselves; registering them only stops a warning per encrypted membership event.
type rawContent map[string]any

func init() {
	for _, t := range []event.Type{
		{Type: LegacyMemberType, Class: event.StateEventType},
		{Type: StickyMemberType, Class: event.MessageEventType},
		{Type: "org.matrix.msc4075.rtc.notification", Class: event.MessageEventType},
		{Type: "org.matrix.msc4310.rtc.decline", Class: event.MessageEventType},
		{Type: "io.element.call.reaction", Class: event.MessageEventType},
		{Type: EncryptionKeysType, Class: event.ToDeviceEventType},
	} {
		if _, known := event.TypeMap[t]; !known {
			event.TypeMap[t] = reflect.TypeOf(rawContent{})
		}
	}
}
