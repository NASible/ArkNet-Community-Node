package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/store"
)

// registerMediaAdministration exposes only ArkNet catalog state. It never
// accepts or returns Node filesystem paths, Jellyfin state, or arbitrary files.
func registerMediaAdministration(admin *http.ServeMux, repo store.MediaCatalogRepository) {
	admin.HandleFunc("POST /api/admin/v1/media/{id}/remove", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !media.ValidMediaID(id) {
			adminError(w, http.StatusBadRequest, "invalid media ID")
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			adminError(w, http.StatusBadRequest, "action does not accept parameters")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if err != nil || len(body) != 0 {
			adminError(w, http.StatusBadRequest, "action requires an empty body")
			return
		}
		if repo == nil {
			adminError(w, http.StatusServiceUnavailable, "media administration unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		object, err := repo.UpdateMediaObjectState(ctx, id, media.ObjectRemoved)
		switch {
		case errors.Is(err, store.ErrNotFound):
			adminError(w, http.StatusNotFound, "media object not found")
		case errors.Is(err, media.ErrInvalidState):
			adminError(w, http.StatusConflict, "media object is already removed")
		case err != nil:
			adminError(w, http.StatusServiceUnavailable, "media administration unavailable")
		default:
			writeAdminJSON(w, http.StatusOK, object.AsAPIView())
		}
	})
}
