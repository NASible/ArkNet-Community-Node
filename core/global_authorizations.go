package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
)

const (
	offlineAuthorizationBodyLimit = 64 << 10
	offlinePermissionMediaAccess  = "MEDIA_ACCESS"
)

func authorizeOfflineNode(w http.ResponseWriter, r *http.Request, body []byte, identities nodes.HandshakeRepository) bool {
	id := r.PathValue("node_id")
	if !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery || identities == nil {
		unauthorizedNode(w)
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	node, key, err := identities.EnrollmentIdentity(ctx, id)
	if errors.Is(err, nodes.ErrNotFound) || errors.Is(err, nodes.ErrInvalidPublicID) {
		unauthorizedNode(w)
		return false
	}
	if err != nil {
		adminError(w, http.StatusServiceUnavailable, "global authorization unavailable")
		return false
	}
	if node.PublicID != id || !verifyNodeProof(r, body, id, key) {
		unauthorizedNode(w)
		return false
	}
	if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
		adminError(w, http.StatusForbidden, "node is not approved")
		return false
	}
	return true
}

// retiredGlobalAuthorizationHandler answers the former Node-relayed password
// check with 410 Gone and never reads a password (ARKSEC-001, Prompt 46B).
// Nodes now verify Core-signed login assertions instead, so a malicious Node
// can no longer use Core as an oracle for a member's global password.
func retiredGlobalAuthorizationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	adminError(w, http.StatusGone, "password relay retired; members sign in at Core")
}

type refreshedAuthorizationView struct {
	MemberID    string           `json:"member_id"`
	Authorized  bool             `json:"authorized"`
	Role        memberships.Role `json:"role,omitempty"`
	Permissions []string         `json:"permissions,omitempty"`
	ValidatedAt string           `json:"validated_at,omitempty"`
	ExpiresAt   string           `json:"expires_at,omitempty"`
}

// globalAuthorizationRefreshHandler lets a reconnected Node refresh only IDs
// it already holds grants for. It carries no username, password, global ID, or
// verifier. expires_at is now plus Core's offline window; the Node applies it
// as shorten-only, so it can revoke or shorten a grant but never extend it.
func globalAuthorizationRefreshHandler(repo memberships.OfflineAuthorizationRepository, identities nodes.HandshakeRepository, window time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, offlineAuthorizationBodyLimit))
		if err != nil {
			adminError(w, http.StatusBadRequest, "global authorization request too large")
			return
		}
		if !authorizeOfflineNode(w, r, body, identities) {
			return
		}
		fields, ok := exactObject(body, "member_ids")
		var memberIDs []string
		if !ok || json.Unmarshal(fields["member_ids"], &memberIDs) != nil || len(memberIDs) < 1 || len(memberIDs) > memberships.MaxAuthorizationRefreshMembers {
			adminError(w, http.StatusBadRequest, "refresh requires a bounded member_ids array")
			return
		}
		seen := map[string]bool{}
		for _, id := range memberIDs {
			if !memberships.ValidMemberID(id) || seen[id] {
				adminError(w, http.StatusBadRequest, "refresh member IDs are invalid")
				return
			}
			seen[id] = true
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		states, err := repo.AuthorizationStates(ctx, r.PathValue("node_id"), memberIDs)
		if err != nil || len(states) != len(memberIDs) {
			adminError(w, http.StatusServiceUnavailable, "global authorization unavailable")
			return
		}
		now := time.Now().UTC().Truncate(time.Second)
		views := make([]refreshedAuthorizationView, 0, len(states))
		for _, state := range states {
			view := refreshedAuthorizationView{MemberID: state.MemberID, Authorized: state.Authorized}
			if state.Authorized {
				view.Role = state.Role
				view.Permissions = []string{offlinePermissionMediaAccess}
				view.ValidatedAt = now.Format(time.RFC3339)
				view.ExpiresAt = now.Add(window).Format(time.RFC3339)
			}
			views = append(views, view)
		}
		writeAdminJSON(w, http.StatusOK, struct {
			Authorizations []refreshedAuthorizationView `json:"authorizations"`
		}{views})
	}
}
