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
	"arknet/core/internal/users"
)

const (
	offlineAuthorizationBodyLimit = 64 << 10
	offlineAuthorizationTTL       = 24 * time.Hour
	offlinePermissionMediaAccess  = "MEDIA_ACCESS"
)

type offlineAuthorizationView struct {
	MemberID    string           `json:"member_id"`
	NodeID      string           `json:"node_id"`
	Role        memberships.Role `json:"role"`
	Permissions []string         `json:"permissions"`
	ValidatedAt string           `json:"validated_at"`
	ExpiresAt   string           `json:"expires_at"`
}

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

func denyGlobalAuthorization(w http.ResponseWriter) {
	adminError(w, http.StatusUnauthorized, "invalid global credentials")
}

// globalAuthorizationHandler verifies the global password inside Core. The
// global ID, username, and Core password hash never cross the Node boundary,
// but the plaintext password currently transits the untrusted Node; ARKSEC-001
// tracks replacing this bootstrap protocol with a Node-bound assertion.
func globalAuthorizationHandler(repo memberships.OfflineAuthorizationRepository, identities nodes.HandshakeRepository) http.HandlerFunc {
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
		fields, ok := exactObject(body, "member_id", "password")
		var memberID, password string
		if !ok || json.Unmarshal(fields["member_id"], &memberID) != nil || json.Unmarshal(fields["password"], &password) != nil ||
			!memberships.ValidMemberID(memberID) || users.ValidatePassword(password) != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			if users.VerifyDummy(ctx, password) != nil {
				adminError(w, http.StatusServiceUnavailable, "global authorization unavailable")
				return
			}
			denyGlobalAuthorization(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		credentials, err := repo.OfflineCredentials(ctx, r.PathValue("node_id"), memberID)
		if errors.Is(err, memberships.ErrAuthorizationDenied) {
			if users.VerifyDummy(ctx, password) != nil {
				adminError(w, http.StatusServiceUnavailable, "global authorization unavailable")
				return
			}
			denyGlobalAuthorization(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "global authorization unavailable")
			return
		}
		matched, err := users.VerifyPassword(ctx, credentials.PasswordHash, password)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "global authorization unavailable")
			return
		}
		if !matched {
			denyGlobalAuthorization(w)
			return
		}
		now := time.Now().UTC().Truncate(time.Second)
		writeAdminJSON(w, http.StatusOK, offlineAuthorizationView{
			MemberID: credentials.MemberID, NodeID: r.PathValue("node_id"), Role: credentials.Role,
			Permissions: []string{offlinePermissionMediaAccess}, ValidatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(offlineAuthorizationTTL).Format(time.RFC3339),
		})
	}
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
// it already cached. It carries no username, password, global ID, or verifier.
func globalAuthorizationRefreshHandler(repo memberships.OfflineAuthorizationRepository, identities nodes.HandshakeRepository) http.HandlerFunc {
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
				view.ExpiresAt = now.Add(offlineAuthorizationTTL).Format(time.RFC3339)
			}
			views = append(views, view)
		}
		writeAdminJSON(w, http.StatusOK, struct {
			Authorizations []refreshedAuthorizationView `json:"authorizations"`
		}{views})
	}
}
