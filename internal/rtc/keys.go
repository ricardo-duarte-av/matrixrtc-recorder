package rtc

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"
)

// MediaKey is one received `io.element.call.encryption_keys` to-device payload.
type MediaKey struct {
	Sender   id.UserID
	RoomID   id.RoomID
	DeviceID id.DeviceID
	MemberID string
	Index    int
	Key      []byte
	// SentTS orders re-deliveries; zero when the sender didn't say.
	SentTS int64
}

type mediaKeyContent struct {
	RoomID id.RoomID `json:"room_id"`
	Keys   *struct {
		Index *int   `json:"index"`
		Key   string `json:"key"`
	} `json:"keys"`
	Member *struct {
		ClaimedDeviceID id.DeviceID `json:"claimed_device_id"`
		ID              string      `json:"id"`
	} `json:"member"`
	SentTS int64 `json:"sent_ts"`
}

// KeyIndexCount is LiveKit's key ring size, which Matrix key indices wrap at.
const KeyIndexCount = 256

// ParseMediaKey reads an `io.element.call.encryption_keys` payload, or returns nil when it's malformed.
func ParseMediaKey(sender id.UserID, raw json.RawMessage) *MediaKey {
	var c mediaKeyContent
	if json.Unmarshal(raw, &c) != nil || c.RoomID == "" || c.Keys == nil || c.Keys.Index == nil || c.Keys.Key == "" ||
		c.Member == nil || c.Member.ClaimedDeviceID == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(c.Keys.Key)
	if err != nil {
		key, err = base64.RawStdEncoding.DecodeString(c.Keys.Key)
		if err != nil {
			return nil
		}
	}
	memberID := c.Member.ID
	if memberID == "" {
		memberID = LegacyIdentity(sender, c.Member.ClaimedDeviceID)
	}
	return &MediaKey{
		Sender:   sender,
		RoomID:   c.RoomID,
		DeviceID: c.Member.ClaimedDeviceID,
		MemberID: memberID,
		Index:    ((*c.Keys.Index % KeyIndexCount) + KeyIndexCount) % KeyIndexCount,
		Key:      key,
		SentTS:   c.SentTS,
	}
}

// KeySink is where keys end up: the recorder's frame decryptors, by SFU participant identity.
type KeySink func(rtcIdentity string, index int, key []byte)

// KeyRing is the receiving half of matrix-js-sdk's RTCEncryptionManager: it maps keys sent by
// (user, device) to the sender's SFU identity, holds keys from members we don't know yet until their
// membership shows up, and ignores older re-deliveries so they can't undo a rotation.
//
// The recorder publishes nothing, so unlike a real participant it never sends a key of its own.
type KeyRing struct {
	roomID id.RoomID
	sink   KeySink

	mu      sync.Mutex
	members map[deviceKey]*Membership
	pending []*MediaKey
	newest  map[slotKey]int64
}

type deviceKey struct {
	user   id.UserID
	device id.DeviceID
}

type slotKey struct {
	identity string
	index    int
}

// pendingLimit bounds keys held for members we haven't seen, so a chatty sender can't grow it forever.
const pendingLimit = 1024

func NewKeyRing(roomID id.RoomID, sink KeySink) *KeyRing {
	return &KeyRing{
		roomID:  roomID,
		sink:    sink,
		members: make(map[deviceKey]*Membership),
		newest:  make(map[slotKey]int64),
	}
}

// OnMembers takes the call's current members (ourselves excluded or not: we never receive our own keys).
func (k *KeyRing) OnMembers(all []*Membership) {
	k.mu.Lock()
	defer k.mu.Unlock()
	members := make(map[deviceKey]*Membership, len(all))
	for _, ms := range all {
		members[deviceKey{ms.UserID, ms.DeviceID}] = ms
	}
	k.members = members
	kept := k.pending[:0]
	for _, mk := range k.pending {
		if !k.deliver(mk) {
			kept = append(kept, mk)
		}
	}
	k.pending = kept
}

// OnKey takes a decrypted `io.element.call.encryption_keys` to-device event.
func (k *KeyRing) OnKey(mk *MediaKey) {
	if mk == nil || mk.RoomID != k.roomID {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.deliver(mk) {
		if len(k.pending) >= pendingLimit {
			k.pending = k.pending[1:]
		}
		k.pending = append(k.pending, mk)
	}
}

func (k *KeyRing) deliver(mk *MediaKey) bool {
	ms, ok := k.members[deviceKey{mk.Sender, mk.DeviceID}]
	if !ok {
		return false
	}
	stamp := mk.SentTS
	if stamp == 0 {
		stamp = time.Now().UnixMilli()
	}
	slot := slotKey{ms.RTCIdentity, mk.Index}
	if prev, seen := k.newest[slot]; seen && prev > stamp {
		return true
	}
	k.newest[slot] = stamp
	k.sink(ms.RTCIdentity, mk.Index, mk.Key)
	return true
}
