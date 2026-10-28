package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/mediaselection"
)

type mediaStorage interface {
	mediaselection.Repository
	EnrollmentIdentity(context.Context) (enrollment.Identity, error)
	TransitionMediaSelection(context.Context, string, mediaselection.State, mediaselection.State) error
}

type mediaOpt struct {
	repository    mediaStorage
	allocation    int64
	coreURL       string
	mediaDir      string
	fetchCatalog  func(context.Context, enrollment.Identity) ([]catalog.Object, error)
	grantTransfer func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error)
	// createInvitation defaults to invitations.Create; tests inject Core.
	createInvitation invitationCreator
}

func (mediaOpt) isOption() {}

type mediaListResponse struct {
	Objects         []mediaselection.Object `json:"objects"`
	AllocationBytes int64                   `json:"allocation_bytes"`
	ReservedBytes   int64                   `json:"reserved_bytes"`
	CoreAvailable   bool                    `json:"core_available"`
}

func mediaJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func mediaError(w http.ResponseWriter, status int, code string) {
	mediaJSON(w, status, map[string]string{"error": code})
}

func (m mediaOpt) identity(ctx context.Context) (enrollment.Identity, error) {
	if m.repository == nil || m.coreURL == "" {
		return enrollment.Identity{}, catalog.ErrRequestFailed
	}
	identity, err := m.repository.EnrollmentIdentity(ctx)
	if err != nil || identity.Status != "APPROVED" || identity.CoreURL != m.coreURL {
		return enrollment.Identity{}, catalog.ErrRequestFailed
	}
	return identity, nil
}

func (m mediaOpt) catalog(ctx context.Context) ([]catalog.Object, error) {
	identity, err := m.identity(ctx)
	if err != nil {
		return nil, catalog.ErrRequestFailed
	}
	fetch := m.fetchCatalog
	if fetch == nil {
		fetch = catalog.Fetch
	}
	return fetch(ctx, identity)
}

func (m mediaOpt) list(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		mediaError(w, 400, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if m.repository == nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	selected, err := m.repository.ListMediaSelections(ctx)
	if err != nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	objects, fetchErr := m.catalog(ctx)
	if fetchErr == nil {
		if err := m.repository.RetainRemovedMediaSelections(ctx, objects); err != nil {
			mediaError(w, 503, "local_media_unavailable")
			return
		}
		selected, err = m.repository.ListMediaSelections(ctx)
		if err != nil {
			mediaError(w, 503, "local_media_unavailable")
			return
		}
	}
	result := mediaListResponse{Objects: make([]mediaselection.Object, 0), AllocationBytes: m.allocation, CoreAvailable: fetchErr == nil}
	byID := make(map[string]mediaselection.Object, len(selected))
	for _, object := range selected {
		if object.LocalState != mediaselection.NotPresent {
			// Fail closed on corrupt or overflowing reservations; never report spare
			// capacity that the durable queue does not actually have.
			if object.Size < 0 || object.Size > math.MaxInt64-result.ReservedBytes {
				mediaError(w, 503, "local_media_unavailable")
				return
			}
			result.ReservedBytes += object.Size
		}
		byID[object.ID] = object
	}
	seen := make(map[string]bool)
	if fetchErr == nil {
		for _, object := range objects {
			if seen[object.ID] {
				continue
			}
			seen[object.ID] = true
			local, ok := byID[object.ID]
			if !ok {
				local = mediaselection.Object{ID: object.ID, Title: object.Title, Size: object.Size, State: object.State, LocalState: mediaselection.NotPresent}
			}
			result.Objects = append(result.Objects, local)
		}
	}
	for _, object := range selected {
		if !seen[object.ID] {
			result.Objects = append(result.Objects, object)
		}
	}
	mediaJSON(w, 200, result)
}

func validSelectionID(id string) bool {
	if len(id) != 70 || !strings.HasPrefix(id, "media_") {
		return false
	}
	for _, c := range id[6:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (m mediaOpt) queue(w http.ResponseWriter, r *http.Request) {
	// No operator-supplied metadata or filesystem paths are accepted. The ID
	// must resolve in a freshly authorized Core catalog before it can be queued.
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" || !validSelectionID(r.PathValue("id")) || r.URL.RawQuery != "" || r.URL.ForceQuery {
		mediaError(w, 400, "invalid_request")
		return
	}
	var body map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	if decoder.Decode(&body) != nil || body == nil || len(body) != 0 || decoder.Decode(new(any)) != io.EOF {
		mediaError(w, 400, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if m.repository == nil {
		mediaError(w, 503, "local_media_unavailable")
		return
	}
	objects, err := m.catalog(ctx)
	if err != nil {
		mediaError(w, 503, "catalog_unavailable")
		return
	}
	for _, object := range objects {
		if object.ID != r.PathValue("id") {
			continue
		}
		if object.State != catalog.Active {
			mediaError(w, 409, "media_removed")
			return
		}
		selected, err := m.repository.QueueMediaSelection(ctx, object, m.allocation)
		switch {
		case errors.Is(err, mediaselection.ErrAllocationExceeded):
			mediaError(w, 409, "storage_allocation_exceeded")
		case errors.Is(err, mediaselection.ErrRemoved):
			mediaError(w, 409, "media_removed")
		case err != nil:
			mediaError(w, 503, "local_media_unavailable")
		default:
			mediaJSON(w, 200, selected)
		}
		return
	}
	mediaError(w, 404, "media_not_found")
}

// deleteRetained removes only the Agent-managed path derived from the opaque
// object ID. Core is not notified of paths or filesystem/Jellyfin details.
func (m mediaOpt) deleteRetained(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSelectionID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
		mediaError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(body) != 0 {
		mediaError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if m.repository == nil || m.mediaDir == "" {
		mediaError(w, http.StatusServiceUnavailable, "local_media_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	selections, err := m.repository.ListMediaSelections(ctx)
	if err != nil {
		mediaError(w, http.StatusServiceUnavailable, "local_media_unavailable")
		return
	}
	found := false
	for _, object := range selections {
		if object.ID == id && object.State == catalog.Removed && object.LocalState == mediaselection.RemovedRetained {
			found = true
			break
		}
	}
	if !found {
		mediaError(w, http.StatusConflict, "media_not_retained")
		return
	}
	path := filepath.Join(m.mediaDir, id+".mp4")
	if info, statErr := os.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() {
			mediaError(w, http.StatusConflict, "media_file_unsafe")
			return
		}
		if err := os.Remove(path); err != nil {
			mediaError(w, http.StatusServiceUnavailable, "local_media_unavailable")
			return
		}
	} else if !os.IsNotExist(statErr) {
		mediaError(w, http.StatusServiceUnavailable, "local_media_unavailable")
		return
	}
	if err := m.repository.TransitionMediaSelection(ctx, id, mediaselection.RemovedRetained, mediaselection.NotPresent); err != nil {
		mediaError(w, http.StatusServiceUnavailable, "local_media_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
