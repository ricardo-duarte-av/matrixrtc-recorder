package rtc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// SFUAccess is where to connect and with what: an SFU WebSocket URL and a LiveKit token for it.
type SFUAccess struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
}

// TokenSubject is who asks for an SFU token: our side of the membership.
type TokenSubject struct {
	UserID   id.UserID
	DeviceID id.DeviceID
	MemberID string
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Transports lists the MSC4143 transports the homeserver advertises, LiveKit ones only.
func Transports(ctx context.Context, cli *mautrix.Client) ([]Transport, error) {
	resp, err := cli.RTCTransports(ctx)
	if err != nil {
		return nil, err
	}
	var out []Transport
	for _, t := range resp.RTCTransports {
		if string(t.Type) == TransportLivekit && t.LivekitServiceURL != "" {
			out = append(out, LivekitTransport(t.LivekitServiceURL))
		}
	}
	return out, nil
}

// GetSFUAccess gets a LiveKit token for a `livekit` transport the way Element Call does: an OpenID
// token from the homeserver taken to the transport's lk-jwt-service — `/sfu/get` for legacy members
// (identity `@user:DEVICE`), `/get_token` for sticky ones (identity = hash of user, device and member).
func GetSFUAccess(ctx context.Context, cli *mautrix.Client, t Transport, roomID id.RoomID, subject TokenSubject, legacy bool) (*SFUAccess, error) {
	service := strings.TrimRight(t.LivekitServiceURL(), "/")
	if t.Type() != TransportLivekit || service == "" {
		return nil, fmt.Errorf("unsupported transport %v", map[string]any(t))
	}
	openID, err := cli.RequestOpenIDToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("request OpenID token: %w", err)
	}
	if legacy {
		return postForAccess(ctx, service+"/sfu/get", map[string]any{
			"room":         roomID,
			"openid_token": openID,
			"device_id":    subject.DeviceID,
		})
	}
	return postForAccess(ctx, service+"/get_token", map[string]any{
		"room_id":      roomID,
		"slot_id":      RoomSlotID,
		"openid_token": openID,
		"member": map[string]any{
			"id":                subject.MemberID,
			"claimed_user_id":   subject.UserID,
			"claimed_device_id": subject.DeviceID,
		},
	})
}

func postForAccess(ctx context.Context, url string, body any) (*SFUAccess, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("SFU token request to %s failed (%d): %.300s", url, resp.StatusCode, data)
	}
	var access SFUAccess
	if err = json.Unmarshal(data, &access); err != nil {
		return nil, fmt.Errorf("parse SFU token response: %w", err)
	}
	if access.URL == "" || access.JWT == "" {
		return nil, fmt.Errorf("SFU token response from %s lacks url or jwt", url)
	}
	return &access, nil
}
