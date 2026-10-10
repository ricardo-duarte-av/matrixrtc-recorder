package rtc

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Fixtures and reference values from xmuks' CallMembershipTest.

func mkEvent(t *testing.T, typ, content string, stateKey *string, sender string, ts int64, stickyMS int64) *event.Event {
	t.Helper()
	raw := map[string]any{
		"type":             typ,
		"room_id":          "!room:example.org",
		"event_id":         fmt.Sprintf("$ev%d", ts),
		"sender":           sender,
		"origin_server_ts": ts,
		"content":          json.RawMessage(content),
	}
	if stateKey != nil {
		raw["state_key"] = *stateKey
	}
	if stickyMS > 0 {
		raw["msc4354_sticky"] = map[string]any{"duration_ms": stickyMS}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var evt event.Event
	if err = json.Unmarshal(data, &evt); err != nil {
		t.Fatal(err)
	}
	return &evt
}

func ptr(s string) *string { return &s }

const alice = "@alice:example.org"

// As Element Call (compatibility mode) sends it.
const legacyJSON = `{"application":"m.call","call_id":"","scope":"m.room","device_id":"ABCDEF",
 "membershipID":"@alice:example.org:ABCDEF","expires":14400000,"m.call.intent":"video",
 "focus_active":{"type":"livekit","focus_selection":"multi_sfu"},
 "foci_preferred":[{"type":"livekit","livekit_service_url":"https://lk.example.org/livekit/jwt"}]}`

const stickyJSON = `{"slot_id":"m.call#ROOM","application":{"type":"m.call","m.call.intent":"audio"},
 "member":{"user_id":"@alice:example.org","device_id":"ABCDEF","id":"b7c3e1f0-1111-4222-8333-944455556666"},
 "transports":{"published":[{"type":"livekit","livekit_service_url":"https://lk"}],"can_subscribe":["livekit"]},
 "versions":[],"msc4354_sticky_key":"b7c3e1f0-1111-4222-8333-944455556666"}`

func TestParseLegacy(t *testing.T) {
	evt := mkEvent(t, LegacyMemberType, legacyJSON, ptr("_@alice:example.org_ABCDEF_m.call"), alice, 1_000_000, 0)
	m := Parse(evt, time.Now())
	if m == nil {
		t.Fatal("expected a membership")
	}
	if m.Format != FormatLegacy || m.SlotID != RoomSlotID || m.DeviceID != "ABCDEF" {
		t.Errorf("unexpected membership %+v", m)
	}
	if m.RTCIdentity != "@alice:example.org:ABCDEF" {
		t.Errorf("identity = %q", m.RTCIdentity)
	}
	if got := m.Transport().LivekitServiceURL(); got != "https://lk.example.org/livekit/jwt" {
		t.Errorf("transport = %q", got)
	}
	if want := time.UnixMilli(1_000_000 + 14_400_000); !m.ExpiresAt.Equal(want) {
		t.Errorf("expires = %v, want %v", m.ExpiresAt, want)
	}
}

func TestLegacyLeave(t *testing.T) {
	evt := mkEvent(t, LegacyMemberType, `{}`, ptr("_@alice:example.org_ABCDEF_m.call"), alice, 1, 0)
	if Parse(evt, time.Now()) != nil {
		t.Error("an empty legacy state event is a leave")
	}
	if KeyOf(evt) != "_@alice:example.org_ABCDEF_m.call" {
		t.Errorf("key = %q", KeyOf(evt))
	}
}

func TestParseStickyHashesIdentityLikeMatrixJSSDK(t *testing.T) {
	evt := mkEvent(t, StickyMemberType, stickyJSON, nil, alice, 1_000_000, 3_600_000)
	m := Parse(evt, time.Now())
	if m == nil {
		t.Fatal("expected a membership")
	}
	if m.Format != FormatSticky || m.Key != "b7c3e1f0-1111-4222-8333-944455556666" || m.Intent != "audio" {
		t.Errorf("unexpected membership %+v", m)
	}
	if m.RTCIdentity != "LirY0rM71dQxYkLKkFvA5f67h5h74g1Chc4+2gnsVCE" {
		t.Errorf("identity = %q", m.RTCIdentity)
	}
	if want := time.UnixMilli(1_000_000 + 3_600_000); !m.ExpiresAt.Equal(want) {
		t.Errorf("expires = %v, want %v", m.ExpiresAt, want)
	}
}

func TestStickyMustBeSentByItsMember(t *testing.T) {
	content := `{"slot_id":"m.call#ROOM","application":{"type":"m.call"},
	 "member":{"user_id":"@mallory:example.org","device_id":"D","id":"x"},
	 "transports":{"published":[],"can_subscribe":[]},"versions":[],"msc4354_sticky_key":"x"}`
	if Parse(mkEvent(t, StickyMemberType, content, nil, alice, 1, 0), time.Now()) != nil {
		t.Error("accepted a membership sent on someone else's behalf")
	}
}

func TestStickyLeaveIsKeyedByMemberID(t *testing.T) {
	evt := mkEvent(t, StickyMemberType, `{"slot_id":"m.call#ROOM","msc4354_sticky_key":"uuid"}`, nil, alice, 1, 0)
	if Parse(evt, time.Now()) != nil || KeyOf(evt) != "uuid" {
		t.Error("a content-less sticky membership is a leave keyed by its member id")
	}
}

func TestOwnMembershipsParseBack(t *testing.T) {
	ts := []Transport{LivekitTransport("https://lk")}
	legacy, _ := json.Marshal(LegacyContent("@me:hs", "DEV", IntentAudio, ts, DefaultLegacyExpiry, nil))
	key := LegacyStateKey("@me:hs", "DEV", "10")
	if key != "_@me:hs_DEV_m.call" {
		t.Errorf("state key = %q", key)
	}
	m := Parse(mkEvent(t, LegacyMemberType, string(legacy), &key, "@me:hs", 1, 0), time.Now())
	if m == nil || m.RTCIdentity != "@me:hs:DEV" || m.SlotID != RoomSlotID {
		t.Errorf("legacy round trip: %+v", m)
	}
	sticky, _ := json.Marshal(StickyContent("@me:hs", "DEV", "uuid", IntentAudio, ts))
	m = Parse(mkEvent(t, StickyMemberType, string(sticky), nil, "@me:hs", 1, 0), time.Now())
	if m == nil || m.RTCIdentity != StickyIdentity("@me:hs", "DEV", "uuid") || m.Key != "uuid" {
		t.Errorf("sticky round trip: %+v", m)
	}
	if LegacyStateKey("@me:hs", "DEV", "org.matrix.msc3757.10") != "@me:hs_DEV_m.call" {
		t.Error("MSC3757 rooms use unprefixed state keys")
	}
}

func TestMembersNewestStickyWinsAndLeaves(t *testing.T) {
	ms := NewMembers()
	now := time.UnixMilli(2_000_000)
	join := mkEvent(t, StickyMemberType, stickyJSON, nil, alice, 1_000_000, 3_600_000)
	leave := mkEvent(t, StickyMemberType, `{"slot_id":"m.call#ROOM","msc4354_sticky_key":"b7c3e1f0-1111-4222-8333-944455556666"}`, nil, alice, 1_500_000, 3_600_000)
	if !ms.Apply(join, now) || len(ms.Active(now)) != 1 {
		t.Fatal("join not applied")
	}
	if !ms.Apply(leave, now) || len(ms.Active(now)) != 0 {
		t.Fatal("leave not applied")
	}
	// A late re-delivery of the older join must not resurrect the member.
	if ms.Apply(join, now) || len(ms.Active(now)) != 0 {
		t.Fatal("older sticky event overrode a newer one")
	}
	if len(ms.Active(time.UnixMilli(1_000_000+3_600_000))) != 0 {
		t.Fatal("expired membership counted")
	}
}

func TestKeyRingHoldsKeysUntilMembershipAndIgnoresStale(t *testing.T) {
	type got struct {
		identity string
		index    int
		key      string
	}
	var delivered []got
	ring := NewKeyRing("!room:example.org", func(identity string, index int, key []byte) {
		delivered = append(delivered, got{identity, index, string(key)})
	})
	keyEvent := func(index int, key string, sentTS int64) *MediaKey {
		raw := fmt.Sprintf(`{"keys":{"index":%d,"key":"%s"},"room_id":"!room:example.org",
		 "member":{"claimed_device_id":"ABCDEF","id":"b7c3e1f0-1111-4222-8333-944455556666"},
		 "session":{"call_id":"","application":"m.call","scope":"m.room"},"sent_ts":%d}`,
			index, b64(key), sentTS)
		return ParseMediaKey(id.UserID(alice), json.RawMessage(raw))
	}

	ring.OnKey(keyEvent(0, "k0", 10))
	if len(delivered) != 0 {
		t.Fatal("delivered a key for an unknown member")
	}
	m := Parse(mkEvent(t, StickyMemberType, stickyJSON, nil, alice, 1, 0), time.Now())
	ring.OnMembers([]*Membership{m})
	if len(delivered) != 1 || delivered[0] != (got{m.RTCIdentity, 0, "k0"}) {
		t.Fatalf("pending key not delivered: %+v", delivered)
	}
	ring.OnKey(keyEvent(1, "k1-new", 30))
	ring.OnKey(keyEvent(1, "k1-old", 20))
	if len(delivered) != 2 || delivered[1].key != "k1-new" {
		t.Fatalf("stale re-delivery not ignored: %+v", delivered)
	}
	other := keyEvent(2, "x", 40)
	other.RoomID = "!elsewhere:example.org"
	ring.OnKey(other)
	if len(delivered) != 2 {
		t.Fatal("accepted a key for another room")
	}
}

func b64(s string) string {
	out, _ := json.Marshal([]byte(s)) // encoding/json writes []byte as standard base64
	return string(out[1 : len(out)-1])
}
