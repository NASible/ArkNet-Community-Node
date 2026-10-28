package main

import (
	"context"
	"net/http"
	"time"

	"arknet/core/internal/nodes"
	"arknet/core/internal/routing"
)

type nodesHandshake = nodes.HandshakeRepository

// missDispatcher records media misses off the routing request path. Each
// record runs in its own goroutine with a bounded timeout; concurrency is
// capped and excess events are dropped, so a slow or failing database can
// never delay or fail a Core fallback.
type missDispatcher struct {
	recorder routing.MissRecorder
	slots    chan struct{}
	timeout  time.Duration
}

const (
	missDispatchConcurrency = 16
	missRecordTimeout       = 3 * time.Second
)

func newMissDispatcher(recorder routing.MissRecorder) *missDispatcher {
	if recorder == nil {
		return nil
	}
	return &missDispatcher{recorder: recorder, slots: make(chan struct{}, missDispatchConcurrency), timeout: missRecordTimeout}
}

// dispatch never blocks. It returns false when the event was dropped.
func (d *missDispatcher) dispatch(nodeID, mediaID string) bool {
	if d == nil {
		return false
	}
	select {
	case d.slots <- struct{}{}:
	default:
		return false
	}
	go func() {
		defer func() { <-d.slots }()
		// Detached from the request: the response has already been written.
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		defer cancel()
		_ = d.recorder.RecordMediaMiss(ctx, nodeID, mediaID)
	}()
	return true
}

type mediaMissView struct {
	MediaID   string `json:"media_id"`
	MissCount int64  `json:"miss_count"`
}

// mediaMissesHandler gives an approved Node its own aggregated misses: object
// ID and count only. No user, member, timestamp, or other Node data.
func mediaMissesHandler(repo routing.MissRepository, identities nodesHandshake) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authorizeMediaNode(w, r, identities) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		misses, err := repo.ListMediaMisses(ctx, r.PathValue("node_id"), routing.MaxListedMisses)
		if err != nil {
			adminError(w, http.StatusServiceUnavailable, "media misses unavailable")
			return
		}
		views := make([]mediaMissView, 0, len(misses))
		for _, miss := range misses {
			views = append(views, mediaMissView{miss.MediaID, miss.MissCount})
		}
		writeAdminJSON(w, http.StatusOK, struct {
			Misses []mediaMissView `json:"misses"`
		}{views})
	}
}
