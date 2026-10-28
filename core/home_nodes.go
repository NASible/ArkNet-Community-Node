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

// homeNodeSource is the only home-node capability given to routing logic:
// read-only and keyed by an already authenticated global user. Routing code
// cannot change a home node, and no Node-facing handler receives it.
type homeNodeSource interface {
	HomeNode(ctx context.Context, userID string) (memberships.HomeNode, error)
}

var errSessionRequired = errors.New("valid session required")

// preferredHomeNode resolves the caller's Core session to a global user and
// returns that user's home node for Core routing decisions. The result
// includes the pairwise member ID for the home Node only; other memberships
// are never loaded.
func preferredHomeNode(ctx context.Context, sessions users.Repository, homes homeNodeSource, sessionDigest [32]byte) (memberships.HomeNode, error) {
	session, err := sessions.GetSession(ctx, sessionDigest)
	if errors.Is(err, users.ErrNotFound) {
		return memberships.HomeNode{}, errSessionRequired
	}
	if err != nil {
		return memberships.HomeNode{}, err
	}
	return homes.HomeNode(ctx, session.UserID)
}

type homeNodeView struct {
	NodeID string `json:"node_id"`
	SetAt  string `json:"set_at"`
}

// registerHomeNodeRoutes exposes the home node only to the authenticated user
// it belongs to. Changing it is an explicit PUT; nothing else writes it.
func registerHomeNodeRoutes(mux *http.ServeMux, sessions users.Repository, homes memberships.HomeNodeRepository) {
	mux.HandleFunc("GET /api/identity/v1/home-node", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		digest, ok := bearerDigest(r)
		if !ok || r.URL.RawQuery != "" || r.URL.ForceQuery {
			sessionRequired(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		home, err := preferredHomeNode(ctx, sessions, homes, digest)
		switch {
		case errors.Is(err, errSessionRequired):
			sessionRequired(w)
		case errors.Is(err, memberships.ErrNoHomeNode):
			adminError(w, http.StatusNotFound, memberships.ErrNoHomeNode.Error())
		case err != nil:
			adminError(w, http.StatusServiceUnavailable, "home node unavailable")
		default:
			writeAdminJSON(w, http.StatusOK, homeNodeView{home.NodeID, home.SetAt.UTC().Format(time.RFC3339)})
		}
	})
	mux.HandleFunc("PUT /api/identity/v1/home-node", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		digest, ok := bearerDigest(r)
		if !ok {
			sessionRequired(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		session, err := sessions.GetSession(ctx, digest)
		if errors.Is(err, users.ErrNotFound) {
			sessionRequired(w)
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "home node unavailable")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256))
		fields, valid := exactObject(body, "node_id")
		var nodeID string
		if err != nil || !valid || r.URL.RawQuery != "" || r.URL.ForceQuery || json.Unmarshal(fields["node_id"], &nodeID) != nil || !nodes.ValidPublicID(nodeID) {
			adminError(w, http.StatusBadRequest, "home node change requires exactly a node_id")
			return
		}
		home, err := homes.SetHomeNode(ctx, session.UserID, nodeID)
		switch {
		case errors.Is(err, memberships.ErrHomeNodeUnavailable):
			// Identical for non-membership, unknown, or untrusted Nodes.
			adminError(w, http.StatusNotFound, memberships.ErrHomeNodeUnavailable.Error())
		case err != nil:
			adminError(w, http.StatusServiceUnavailable, "home node unavailable")
		default:
			writeAdminJSON(w, http.StatusOK, homeNodeView{home.NodeID, home.SetAt.UTC().Format(time.RFC3339)})
		}
	})
}
