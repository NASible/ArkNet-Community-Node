package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/mediamiss"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

type mediaMissOpt struct{ repository mediamiss.Repository }

func (mediaMissOpt) isOption() {}

func readExactJSON(w http.ResponseWriter, r *http.Request, keys ...string) (map[string]json.RawMessage, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256))
	if err != nil {
		return nil, false
	}
	return exactLocalObject(body, keys...)
}

func registerMediaMisses(mux *routeMux, opt mediaMissOpt) {
	repo := opt.repository
	unavailable := func(w http.ResponseWriter) { mediaError(w, http.StatusServiceUnavailable, "media_misses_unavailable") }
	mux.HandleFunc("GET /api/v1/settings/media-miss-policy", func(w http.ResponseWriter, r *http.Request) {
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		policy, err := repo.MediaMissPolicy(ctx)
		if err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusOK, map[string]mediamiss.Policy{"policy": policy})
	})
	mux.Handle("PUT /api/v1/settings/media-miss-policy", http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields, ok := readExactJSON(w, r, "policy")
		var policy mediamiss.Policy
		if !ok || json.Unmarshal(fields["policy"], &policy) != nil || !mediamiss.ValidPolicy(policy) {
			mediaError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := repo.SetMediaMissPolicy(ctx, policy); err != nil {
			unavailable(w)
			return
		}
		mediaJSON(w, http.StatusOK, map[string]mediamiss.Policy{"policy": policy})
	})))
	mux.HandleFunc("GET /api/v1/media-misses", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			mediaError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		policy, err := repo.MediaMissPolicy(ctx)
		if err != nil {
			unavailable(w)
			return
		}
		notifications := []mediamiss.Notification{}
		// MANUAL means the operator asked not to be notified.
		if policy != mediamiss.Manual {
			if notifications, err = repo.PendingMediaMisses(ctx); err != nil {
				unavailable(w)
				return
			}
		}
		mediaJSON(w, http.StatusOK, struct {
			Policy        mediamiss.Policy         `json:"policy"`
			Notifications []mediamiss.Notification `json:"notifications"`
		}{policy, notifications})
	})
	mux.Handle("POST /api/v1/media-misses/{id}/ignore", http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fields, ok := readExactJSON(w, r)
		if !ok || len(fields) != 0 || !validSelectionID(r.PathValue("id")) {
			mediaError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if repo == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		switch err := repo.IgnoreMediaMiss(ctx, r.PathValue("id")); {
		case errors.Is(err, mediamiss.ErrNotFound):
			mediaError(w, http.StatusNotFound, "media_miss_not_found")
		case err != nil:
			unavailable(w)
		default:
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusNoContent)
		}
	})))
}

// mediaMissSyncer pulls this Node's aggregated misses from Core outbound and
// applies the local policy. It never runs on a playback path: Core has
// already served the fallback before a miss is visible here.
type mediaMissSyncer struct {
	media       mediaOpt
	repository  mediamiss.Repository
	transfers   transferOpt
	fetchMisses func(context.Context, enrollment.Identity) ([]catalog.Miss, error)
}

func (s mediaMissSyncer) once(ctx context.Context) error {
	if s.repository == nil || s.media.repository == nil {
		return nil
	}
	policy, err := s.repository.MediaMissPolicy(ctx)
	if err != nil || policy == mediamiss.Manual {
		return err
	}
	identity, err := s.media.identity(ctx)
	if err != nil {
		return nil // not enrolled/approved yet, or federation disabled
	}
	fetch := s.fetchMisses
	if fetch == nil {
		fetch = catalog.FetchMediaMisses
	}
	misses, err := fetch(ctx, identity)
	if err != nil {
		return err
	}
	objects, err := s.media.catalog(ctx)
	if err != nil {
		return err
	}
	active := make(map[string]catalog.Object, len(objects))
	for _, object := range objects {
		if object.State == catalog.Active && len(object.InfoHashV2) == 64 {
			active[object.ID] = object
		}
	}
	notifications := make([]mediamiss.Notification, 0, len(misses))
	for _, miss := range misses {
		// Object availability: only still-active, transferable catalog objects.
		if object, ok := active[miss.MediaID]; ok {
			notifications = append(notifications, mediamiss.Notification{MediaID: object.ID, Title: object.Title, Size: object.Size, MissCount: miss.MissCount})
		}
	}
	if err := s.repository.StoreMediaMisses(ctx, notifications); err != nil {
		return err
	}
	if policy == mediamiss.Automatic {
		return s.automatic(ctx, active)
	}
	return nil
}

// automatic queues and starts at most one missed object per pass, and only
// when no real transfer is in flight (bandwidth policy). Queuing uses the
// normal allocation-checked path (storage quota); starting uses the normal
// Core-authorized start path (availability). Without a transfer engine it does
// nothing, rather than reserving storage that will never be filled.
func (s mediaMissSyncer) automatic(ctx context.Context, active map[string]catalog.Object) error {
	if s.transfers.service == nil {
		return nil
	}
	statuses, err := s.transfers.service.List(ctx)
	if err != nil {
		return err
	}
	for _, status := range statuses {
		if !status.Mock && (status.State == transfer.Downloading || status.State == transfer.Paused) {
			return nil
		}
	}
	pending, err := s.repository.PendingMediaMisses(ctx)
	if err != nil {
		return err
	}
	for _, notification := range pending {
		object, ok := active[notification.MediaID]
		if !ok {
			continue
		}
		selection, err := s.media.repository.QueueMediaSelection(ctx, object, s.media.allocation)
		if errors.Is(err, mediaselection.ErrAllocationExceeded) || errors.Is(err, mediaselection.ErrRemoved) {
			continue // leave it for the operator; try a smaller candidate
		}
		if err != nil {
			return err
		}
		if err := s.transfers.startSelection(ctx, selection); err != nil {
			return fmt.Errorf("automatic start: %w", err)
		}
		return nil
	}
	return nil
}

func runMediaMissSync(ctx context.Context, interval time.Duration, syncer mediaMissSyncer, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if interval <= 0 {
			interval = 30 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := syncer.once(callCtx)
			cancel()
			if err != nil && onError != nil {
				onError(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
