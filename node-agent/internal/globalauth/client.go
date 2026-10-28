package globalauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
)

type authorizationWire struct {
	MemberID    string   `json:"member_id"`
	NodeID      string   `json:"node_id"`
	Role        string   `json:"role"`
	Permissions []string `json:"permissions"`
	ValidatedAt string   `json:"validated_at"`
	ExpiresAt   string   `json:"expires_at"`
}

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

func signedPost(ctx context.Context, identity enrollment.Identity, suffix string, body []byte) (*http.Response, error) {
	if ctx.Err() != nil || identity.Status != "APPROVED" || identity.CoreURL == "" || !ValidNodeID(identity.NodeID) {
		return nil, ErrUnavailable
	}
	endpoint := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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

// Authorize asks Core first. ErrDenied is authoritative and must never fall
// back to cache; only ErrUnavailable permits bounded offline verification.
func Authorize(ctx context.Context, identity enrollment.Identity, memberID, password string) (Authorization, error) {
	body, _ := json.Marshal(struct {
		MemberID string `json:"member_id"`
		Password string `json:"password"`
	}{memberID, password})
	resp, err := signedPost(ctx, identity, "/offline-authorizations", body)
	if err != nil {
		return Authorization{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Authorization{}, ErrDenied
	}
	if resp.StatusCode >= 500 {
		return Authorization{}, ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return Authorization{}, ErrInvalidResponse
	}
	var wire authorizationWire
	if readJSON(resp, &wire) != nil {
		return Authorization{}, ErrInvalidResponse
	}
	validated, e1 := time.Parse(time.RFC3339, wire.ValidatedAt)
	expires, e2 := time.Parse(time.RFC3339, wire.ExpiresAt)
	a := Authorization{MemberID: wire.MemberID, NodeID: wire.NodeID, Role: wire.Role, Permissions: wire.Permissions, ValidatedAt: validated, ExpiresAt: expires, PasswordHash: "pending"}
	if e1 != nil || e2 != nil || wire.MemberID != memberID || wire.NodeID != identity.NodeID || !ValidAuthorization(a, time.Now()) {
		return Authorization{}, ErrInvalidResponse
	}
	a.PasswordHash = ""
	return a, nil
}

// Refresh checks every cached pairwise member after an accepted heartbeat.
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
	resp, err := signedPost(ctx, identity, "/offline-authorizations/refresh", body)
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
			probe := Authorization{MemberID: wire.MemberID, NodeID: identity.NodeID, Role: wire.Role, Permissions: wire.Permissions, ValidatedAt: validated, ExpiresAt: expires, PasswordHash: "present"}
			if e1 != nil || e2 != nil || !ValidAuthorization(probe, now) {
				return nil, ErrInvalidResponse
			}
			state.Role, state.Permissions, state.ValidatedAt, state.ExpiresAt = wire.Role, wire.Permissions, validated, expires
		}
		states = append(states, state)
	}
	return states, nil
}
