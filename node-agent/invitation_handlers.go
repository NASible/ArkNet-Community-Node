package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/invitations"
)

type invitationResponse struct {
	InvitationID string    `json:"invitation_id"`
	Secret       string    `json:"secret"`
	Role         string    `json:"role"`
	MaxUses      int       `json:"max_uses"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// invite is the local operator action. Core is authoritative for policy; the
// Agent forwards only role, expiry, and max uses, and never persists secrets.
func (m mediaOpt) invite(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		mediaError(w, http.StatusUnsupportedMediaType, "json_required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512))
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request struct {
		Role      *string    `json:"role"`
		ExpiresAt *time.Time `json:"expires_at"`
		MaxUses   *int       `json:"max_uses"`
	}
	if err != nil || decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF ||
		request.Role == nil || request.ExpiresAt == nil || request.MaxUses == nil || r.URL.RawQuery != "" {
		mediaError(w, http.StatusBadRequest, "invalid_invitation_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	identity, err := m.identity(ctx)
	if err != nil {
		mediaError(w, http.StatusServiceUnavailable, "core_unavailable")
		return
	}
	create := m.createInvitation
	if create == nil {
		create = invitations.Create
	}
	invitation, err := create(ctx, identity, invitations.Request{Role: *request.Role, ExpiresAt: *request.ExpiresAt, MaxUses: *request.MaxUses})
	switch {
	case errors.Is(err, invitations.ErrRejected):
		mediaError(w, http.StatusBadRequest, "invalid_invitation_policy")
		return
	case errors.Is(err, invitations.ErrNotApproved):
		mediaError(w, http.StatusConflict, "node_not_approved")
		return
	case err != nil:
		mediaError(w, http.StatusServiceUnavailable, "core_unavailable")
		return
	}
	mediaJSON(w, http.StatusCreated, invitationResponse{invitation.InvitationID, invitation.Secret, invitation.Role, invitation.MaxUses, invitation.ExpiresAt})
}

type invitationCreator = func(context.Context, enrollment.Identity, invitations.Request) (invitations.Invitation, error)
