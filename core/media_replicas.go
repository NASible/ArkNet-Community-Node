package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/store"
)

const mediaReplicaBodyLimit = 1024

type mediaReplicaReport struct {
	Size       int64  `json:"size"`
	InfoHashV2 string `json:"info_hash_v2"`
}

// mediaReplicaHandler records an authenticated node's assertion that it has
// verified and retained the current bytes for an active catalog object.
func mediaReplicaHandler(catalog store.MediaCatalogRepository, replicas store.MediaReplicaRepository, identities nodes.HandshakeRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "replica report does not accept parameters", http.StatusBadRequest)
			return
		}
		contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || contentType != "application/json" {
			http.Error(w, "JSON content type required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mediaReplicaBodyLimit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "replica report too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "invalid replica report", http.StatusBadRequest)
			}
			return
		}

		fields, exact := exactObject(body, "size", "info_hash_v2")
		var report mediaReplicaReport
		mediaID := r.PathValue("id")
		if !exact || json.Unmarshal(fields["size"], &report.Size) != nil || json.Unmarshal(fields["info_hash_v2"], &report.InfoHashV2) != nil || report.Size < 0 || !validInfoHashV2(report.InfoHashV2) || !media.ValidMediaID(mediaID) {
			http.Error(w, "invalid replica report", http.StatusBadRequest)
			return
		}
		if identities == nil || replicas == nil || catalog == nil {
			http.Error(w, "replica reporting unavailable", http.StatusServiceUnavailable)
			return
		}

		nodeID := r.PathValue("node_id")
		if !nodes.ValidPublicID(nodeID) {
			unauthorizedNode(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		node, key, err := identities.EnrollmentIdentity(ctx, nodeID)
		if errors.Is(err, nodes.ErrNotFound) || errors.Is(err, nodes.ErrInvalidPublicID) {
			unauthorizedNode(w)
			return
		}
		if err != nil {
			http.Error(w, "replica reporting unavailable", http.StatusServiceUnavailable)
			return
		}
		if node.PublicID != nodeID || !verifyNodeProof(r, body, nodeID, key) {
			unauthorizedNode(w)
			return
		}
		if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
			http.Error(w, "node is not approved", http.StatusForbidden)
			return
		}

		object, err := catalog.GetMediaObjectByPublicID(ctx, mediaID)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "media object not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "replica reporting unavailable", http.StatusServiceUnavailable)
			return
		}
		if object.State != media.ObjectActive || object.Size != report.Size || object.InfoHashV2 != report.InfoHashV2 {
			http.Error(w, "replica does not match active media object", http.StatusConflict)
			return
		}

		_, err = replicas.ReportMediaReplicaAvailable(ctx, nodeID, mediaID, report.Size, report.InfoHashV2)
		if errors.Is(err, store.ErrReplicaNotEligible) {
			http.Error(w, "replica is not eligible", http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, "replica reporting unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
