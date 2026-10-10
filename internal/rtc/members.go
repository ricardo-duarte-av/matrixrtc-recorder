package rtc

import (
	"sort"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Members tracks who is in a room's call: legacy state memberships and sticky memberships merged,
// keyed by the slot each occupies (state key / sticky key), newest event per key winning.
type Members struct {
	mu    sync.Mutex
	byKey map[string]memberEntry
}

type memberEntry struct {
	ts         int64
	membership *Membership
}

func NewMembers() *Members {
	return &Members{byKey: make(map[string]memberEntry)}
}

// Apply takes a single membership event (join or leave). True when the call's members changed.
func (m *Members) Apply(evt *event.Event, now time.Time) bool {
	key := KeyOf(evt)
	if key == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	old, had := m.byKey[key]
	// State is authoritative whatever its timestamp; sticky events race, so the newest wins.
	if evt.Type.Type == StickyMemberType && had && old.ts > evt.Timestamp {
		return false
	}
	membership := Parse(evt, now)
	m.byKey[key] = memberEntry{ts: evt.Timestamp, membership: membership}
	return !had || !sameMembership(old.membership, membership)
}

func sameMembership(a, b *Membership) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.EventID == b.EventID
}

// Active lists the live members of the room-wide `m.call`, oldest join first, one per user+device.
func (m *Members) Active(now time.Time) []*Membership {
	m.mu.Lock()
	defer m.mu.Unlock()
	type device struct {
		user   id.UserID
		device id.DeviceID
	}
	newest := make(map[device]*Membership)
	for _, e := range m.byKey {
		ms := e.membership
		if ms == nil || ms.Expired(now) || ms.SlotID != RoomSlotID {
			continue
		}
		k := device{ms.UserID, ms.DeviceID}
		if cur, ok := newest[k]; !ok || ms.CreatedTS.After(cur.CreatedTS) {
			newest[k] = ms
		}
	}
	out := make([]*Membership, 0, len(newest))
	for _, ms := range newest {
		out = append(out, ms)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedTS.Before(out[j].CreatedTS) })
	return out
}

// Others is Active without the given device (normally our own).
func Others(all []*Membership, userID id.UserID, deviceID id.DeviceID) []*Membership {
	out := make([]*Membership, 0, len(all))
	for _, ms := range all {
		if ms.UserID != userID || ms.DeviceID != deviceID {
			out = append(out, ms)
		}
	}
	return out
}
