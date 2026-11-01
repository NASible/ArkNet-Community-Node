package globalauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
)

// MaxAssertionKeys bounds Core's published key set (one active key plus up
// to eight published-only keys).
const MaxAssertionKeys = 9

type refreshWire struct {
	MemberID    string   `json:"member_id"`
	Authorized  bool     `json:"authorized"`
	Role        string   `json:"role,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	ValidatedAt string   `json:"validated_at,omitempty"`
	ExpiresAt   string   `json:"expires_at,omitempty"`
}

func client() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, DisableCompression: true}, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func signedRequest(ctx context.Context, identity enrollment.Identity, method, suffix string, body []byte) (*http.Response, error) {
	if ctx.Err() != nil || identity.Status != "APPROVED" || identity.CoreURL == "" || !ValidNodeID(identity.NodeID) {
		return nil, ErrUnavailable
	}
	endpoint := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + suffix
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" || enrollment.SignRequest(req, identity.NodeID, identity.PrivateSeed, body) != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	return resp, nil
}

func readJSON(resp *http.Response, target any) error {
	mediaType, _, typeErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if typeErr != nil || mediaType != "application/json" || readErr != nil || len(data) > 64<<10 || decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrInvalidResponse
	}
	return nil
}

// FetchAssertionKeys reads Core's published assertion key set from the
// configured Core URL (decision Q4). Transport failures and 5xx responses are
// ErrUnavailable; anything else that is not a well-formed set is
// ErrInvalidResponse and never counts as an outage.
func FetchAssertionKeys(ctx context.Context, coreURL string) ([]VerificationKey, error) {
	if coreURL == "" {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(coreURL, "/")+"/api/federation/v1/assertion-keys", nil)
	if err != nil {
		return nil, ErrInvalidResponse
	}
	resp, err := client().Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrInvalidResponse
	}
	var wire struct {
		Keys []struct {
			KeyID     string `json:"key_id"`
			Algorithm string `json:"algorithm"`
			PublicKey string `json:"public_key"`
			Status    string `json:"status"`
		} `json:"keys"`
	}
	if readJSON(resp, &wire) != nil || len(wire.Keys) < 1 || len(wire.Keys) > MaxAssertionKeys {
		return nil, ErrInvalidResponse
	}
	keys := make([]VerificationKey, 0, len(wire.Keys))
	seen := map[KeyID]bool{}
	active := 0
	for _, entry := range wire.Keys {
		raw, err := base64.RawURLEncoding.Strict().DecodeString(entry.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || entry.Algorithm != "Ed25519" {
			return nil, ErrInvalidResponse
		}
		id := KeyIDFor(raw)
		if id.String() != entry.KeyID || seen[id] {
			return nil, ErrInvalidResponse
		}
		switch entry.Status {
		case "active":
			active++
		case "published":
		default:
			return nil, ErrInvalidResponse
		}
		seen[id] = true
		keys = append(keys, VerificationKey{ID: id, PublicKey: ed25519.PublicKey(raw)})
	}
	if active != 1 {
		return nil, ErrInvalidResponse
	}
	return keys, nil
}

// RegisterLoginCallbacks replaces this Node's login callback URLs at Core over
// the signed federation channel (decision Q2).
func RegisterLoginCallbacks(ctx context.Context, identity enrollment.Identity, urls []string) error {
	body, _ := json.Marshal(struct {
		CallbackURLs []string `json:"callback_urls"`
	}{urls})
	resp, err := signedRequest(ctx, identity, http.MethodPut, "/login-callbacks", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode >= 500:
		return ErrUnavailable
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrDenied
	}
	return ErrInvalidResponse
}

// Refresh checks every cached pairwise member after an accepted heartbeat.
// The result may only shorten or revoke grants (ApplyRefresh enforces this).
func Refresh(ctx context.Context, identity enrollment.Identity, memberIDs []string) ([]RefreshState, error) {
	if len(memberIDs) < 1 || len(memberIDs) > MaxCachedMembers {
		return nil, ErrInvalidResponse
	}
	seen := map[string]bool{}
	for _, id := range memberIDs {
		if !ValidMemberID(id) || seen[id] {
			return nil, ErrInvalidResponse
		}
		seen[id] = true
	}
	body, _ := json.Marshal(struct {
		MemberIDs []string `json:"member_ids"`
	}{memberIDs})
	resp, err := signedRequest(ctx, identity, http.MethodPost, "/offline-authorizations/refresh", body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrInvalidResponse
	}
	var response struct {
		Authorizations []refreshWire `json:"authorizations"`
	}
	if readJSON(resp, &response) != nil || len(response.Authorizations) != len(memberIDs) {
		return nil, ErrInvalidResponse
	}
	states := make([]RefreshState, 0, len(memberIDs))
	now := time.Now()
	for i, wire := range response.Authorizations {
		if wire.MemberID != memberIDs[i] || !ValidMemberID(wire.MemberID) {
			return nil, ErrInvalidResponse
		}
		state := RefreshState{MemberID: wire.MemberID, Authorized: wire.Authorized}
		if !wire.Authorized {
			if wire.Role != "" || len(wire.Permissions) != 0 || wire.ValidatedAt != "" || wire.ExpiresAt != "" {
				return nil, ErrInvalidResponse
			}
		} else {
			validated, e1 := time.Parse(time.RFC3339, wire.ValidatedAt)
			expires, e2 := time.Parse(time.RFC3339, wire.ExpiresAt)
			if e1 != nil || e2 != nil || !ValidRefresh(wire.Role, wire.Permissions, validated, expires, now) {
				return nil, ErrInvalidResponse
			}
			state.Role, state.Permissions, state.ValidatedAt, state.ExpiresAt = wire.Role, wire.Permissions, validated, expires
		}
		states = append(states, state)
	}
	return states, nil
}

// ValidRefresh bounds an authorized refresh state by D3.
func ValidRefresh(role string, permissions []string, validated, expires, now time.Time) bool {
	return ValidRole(role) && ValidPermissions(permissions) && !validated.IsZero() && !validated.After(now.Add(5*time.Minute)) &&
		expires.After(validated) && expires.Sub(validated) <= MaxOfflineWindow && expires.After(now)
}
