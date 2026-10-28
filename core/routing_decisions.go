package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/memberships"
	"arknet/core/internal/routing"
	"arknet/core/internal/store"
	"arknet/core/internal/users"
)

type routingDecisionView struct {
	Target routing.Target `json:"target"`
	NodeID string         `json:"node_id,omitempty"`
}

// mediaRoutingHandler answers, for the authenticated user only, whether to
// obtain an active media object from their home Node or from Core. The
// response carries no reason, health detail, replica list, member ID, or other
// Node identity, and no Node is told that a decision was made.
func mediaRoutingHandler(catalog store.MediaCatalogRepository, sessions users.Repository, homes homeNodeSource, routes routing.Repository, misses *missDispatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Add("Vary", "Authorization")
		digest, ok := bearerDigest(r)
		if !ok {
			sessionRequired(w)
			return
		}
		mediaID := r.PathValue("media_id")
		if r.URL.RawQuery != "" || r.URL.ForceQuery || !media.ValidMediaID(mediaID) {
			adminError(w, http.StatusBadRequest, "routing requires exactly a valid media ID")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// Authenticate before touching the catalog so unauthenticated callers
		// cannot use routing to probe media IDs.
		home, err := preferredHomeNode(ctx, sessions, homes, digest)
		switch {
		case errors.Is(err, errSessionRequired):
			sessionRequired(w)
			return
		case errors.Is(err, memberships.ErrNoHomeNode):
			home = memberships.HomeNode{}
		case err != nil:
			adminError(w, http.StatusServiceUnavailable, "routing unavailable")
			return
		}
		object, err := catalog.GetMediaObjectByPublicID(ctx, mediaID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && object.State != media.ObjectActive) {
			adminError(w, http.StatusNotFound, "media unavailable")
			return
		}
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "routing unavailable")
			return
		}
		var status routing.HomeNodeStatus
		if home.NodeID != "" {
			status, err = routes.HomeNodeStatus(ctx, home.NodeID, mediaID, routing.MaxHeartbeatAge)
			if err != nil {
				adminError(w, http.StatusServiceUnavailable, "routing unavailable")
				return
			}
		}
		decision := routing.Decide(home.NodeID, status)
		writeAdminJSON(w, http.StatusOK, routingDecisionView{decision.Target, decision.NodeID})
		// Recorded only after the response is written, and never awaited.
		if routing.IsMiss(home.NodeID, status) {
			misses.dispatch(home.NodeID, mediaID)
		}
	}
}
