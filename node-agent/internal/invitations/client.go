// Package invitations asks Core, as this signed Node, to mint a membership
// invitation. The request names no user, and the Agent never stores the
// returned secret: the operator shares it out of band exactly once.
package invitations

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
)

var (
	ErrRequestFailed = errors.New("invitation_request_failed")
	ErrRejected      = errors.New("invitation_policy_rejected")
	ErrNotApproved   = errors.New("node_not_approved")
)

type Request struct {
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at"`
	MaxUses   int       `json:"max_uses"`
}

type Invitation struct {
	InvitationID string    `json:"invitation_id"`
	Secret       string    `json:"secret"`
	NodeID       string    `json:"node_id"`
	Role         string    `json:"role"`
	MaxUses      int       `json:"max_uses"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func boundedClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Create signs a POST to Core's node-scoped invitation endpoint and strictly
// validates the reply against what was requested.
func Create(ctx context.Context, identity enrollment.Identity, request Request) (Invitation, error) {
	if ctx.Err() != nil || identity.Status != "APPROVED" || identity.CoreURL == "" {
		return Invitation{}, ErrRequestFailed
	}
	body, err := json.Marshal(struct {
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
		MaxUses   int    `json:"max_uses"`
	}{request.Role, request.ExpiresAt.UTC().Format(time.RFC3339), request.MaxUses})
	if err != nil {
		return Invitation{}, ErrRequestFailed
	}
	endpoint := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + "/invitations"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" ||
		enrollment.SignRequest(req, identity.NodeID, identity.PrivateSeed, body) != nil {
		return Invitation{}, ErrRequestFailed
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := boundedClient().Do(req)
	if err != nil {
		return Invitation{}, ErrRequestFailed
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusBadRequest:
		return Invitation{}, ErrRejected
	case http.StatusForbidden:
		return Invitation{}, ErrNotApproved
	default:
		return Invitation{}, ErrRequestFailed
	}
	mediaType, _, typeErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2049))
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var invitation Invitation
	if typeErr != nil || mediaType != "application/json" || readErr != nil || len(data) > 2048 ||
		decoder.Decode(&invitation) != nil || decoder.Decode(new(any)) != io.EOF {
		return Invitation{}, ErrRequestFailed
	}
	raw, secretErr := base64.RawURLEncoding.Strict().DecodeString(invitation.Secret)
	if secretErr != nil || len(raw) != 32 || !validInvitationID(invitation.InvitationID) ||
		invitation.NodeID != identity.NodeID || invitation.Role != request.Role || invitation.MaxUses != request.MaxUses ||
		!invitation.ExpiresAt.Equal(request.ExpiresAt.UTC().Truncate(time.Second)) {
		return Invitation{}, ErrRequestFailed
	}
	return invitation, nil
}

func validInvitationID(id string) bool {
	if len(id) != 71 || !strings.HasPrefix(id, "invite_") {
		return false
	}
	for _, c := range id[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
