package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/users"
)

const invitationBodyLimit = 512

// invitationCreateHandler lets an approved Node (its operator acting through
// the signed Node identity) invite to itself only. The request names no user,
// and the response contains no user data: operators cannot address, search,
// or enumerate global users through this API.
func invitationCreateHandler(repo memberships.Repository, identities nodes.HandshakeRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("node_id")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, invitationBodyLimit))
		if err != nil {
			adminError(w, http.StatusBadRequest, "invitation request too large")
			return
		}
		if !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery || identities == nil {
			unauthorizedNode(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		node, key, err := identities.EnrollmentIdentity(ctx, id)
		if errors.Is(err, nodes.ErrNotFound) || errors.Is(err, nodes.ErrInvalidPublicID) {
			unauthorizedNode(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "invitations unavailable")
			return
		}
		if node.PublicID != id || !verifyNodeProof(r, body, id, key) {
			unauthorizedNode(w)
			return
		}
		if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
			adminError(w, http.StatusForbidden, "node is not approved")
			return
		}
		fields, ok := exactObject(body, "role", "expires_at", "max_uses")
		var role, expiresValue string
		var maxUses int
		if !ok || json.Unmarshal(fields["role"], &role) != nil || json.Unmarshal(fields["expires_at"], &expiresValue) != nil ||
			json.Unmarshal(fields["max_uses"], &maxUses) != nil {
			adminError(w, http.StatusBadRequest, "invitation requires exactly role, expires_at and max_uses")
			return
		}
		expiresAt, err := time.Parse(time.RFC3339, expiresValue)
		if err != nil {
			adminError(w, http.StatusBadRequest, "expires_at must be an RFC 3339 timestamp")
			return
		}
		switch memberships.ValidatePolicy(memberships.Role(role), expiresAt, maxUses, time.Now()) {
		case nil:
		case memberships.ErrInvalidRole:
			adminError(w, http.StatusBadRequest, "role must be MEMBER or ADMIN")
			return
		case memberships.ErrInvalidExpiration:
			adminError(w, http.StatusBadRequest, "expires_at must be between 1 minute and 30 days from now")
			return
		default:
			adminError(w, http.StatusBadRequest, "max_uses must be between 1 and 100")
			return
		}
		secret, digest, err := memberships.NewSecret()
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "invitations unavailable")
			return
		}
		// Ed25519 signatures are deterministic: an identical replay within the
		// proof window has an identical signature and is rejected by the store.
		proof := sha256.Sum256([]byte(r.Header.Get("X-ArkNet-Node-Signature")))
		invitation, err := repo.CreateInvitation(ctx, id, memberships.Role(role), expiresAt, maxUses, digest, proof)
		switch {
		case errors.Is(err, memberships.ErrReplayedCreate):
			adminError(w, http.StatusConflict, "invitation request was already processed")
			return
		case errors.Is(err, memberships.ErrNodeNotFound):
			adminError(w, http.StatusForbidden, "node is not approved")
			return
		case err != nil:
			adminError(w, http.StatusServiceUnavailable, "invitations unavailable")
			return
		}
		writeAdminJSON(w, http.StatusCreated, struct {
			InvitationID string           `json:"invitation_id"`
			Secret       string           `json:"secret"`
			NodeID       string           `json:"node_id"`
			Role         memberships.Role `json:"role"`
			MaxUses      int              `json:"max_uses"`
			ExpiresAt    string           `json:"expires_at"`
		}{invitation.ID, secret, invitation.NodeID, invitation.Role, invitation.MaxUses, invitation.ExpiresAt.UTC().Format(time.RFC3339)})
	}
}

// invitationAcceptHandler requires a Core session: the user authenticates to
// Core, never to the inviting Node, and the secret travels only in the body.
func invitationAcceptHandler(repo memberships.Repository, identities users.Repository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		sessionDigest, ok := bearerDigest(r)
		if !ok {
			sessionRequired(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := identities.GetSession(ctx, sessionDigest)
		if errors.Is(err, users.ErrNotFound) {
			sessionRequired(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "invitations unavailable")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, invitationBodyLimit))
		fields, valid := exactObject(body, "secret")
		var secret string
		if err != nil || !valid || r.URL.RawQuery != "" || json.Unmarshal(fields["secret"], &secret) != nil {
			adminError(w, http.StatusBadRequest, "acceptance requires exactly a secret string")
			return
		}
		secretDigest, ok := memberships.SecretDigest(secret)
		if !ok {
			adminError(w, http.StatusNotFound, memberships.ErrUnavailable.Error())
			return
		}
		membership, err := repo.AcceptInvitation(ctx, secretDigest, session.UserID)
		switch {
		case errors.Is(err, memberships.ErrUnavailable):
			adminError(w, http.StatusNotFound, memberships.ErrUnavailable.Error())
			return
		case errors.Is(err, memberships.ErrAlreadyMember):
			adminError(w, http.StatusConflict, memberships.ErrAlreadyMember.Error())
			return
		case errors.Is(err, users.ErrInactive):
			sessionRequired(w)
			return
		case err != nil:
			adminError(w, http.StatusServiceUnavailable, "invitations unavailable")
			return
		}
		writeAdminJSON(w, http.StatusCreated, struct {
			NodeID   string           `json:"node_id"`
			MemberID string           `json:"member_id"`
			Role     memberships.Role `json:"role"`
			JoinedAt string           `json:"joined_at"`
		}{membership.NodeID, membership.MemberID, membership.Role, membership.CreatedAt.UTC().Format(time.RFC3339)})
	}
}
