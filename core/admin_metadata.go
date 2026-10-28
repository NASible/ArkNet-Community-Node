package main

import (
	"arknet/core/internal/nodes"
	"context"
	"io"
	"net/http"
	"time"
)

// Dedicated view deliberately omits row IDs, keys, proof timestamps and creation
// metadata. Keep pending listing's original response contract unchanged.
func administrativeMetadataHandler(repo nodes.MetadataRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("node_id")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if r.Method != http.MethodGet || !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery || err != nil || len(body) != 0 {
			adminError(w, 400, "invalid request")
			return
		}
		if repo == nil {
			adminError(w, 503, "administration unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		node, err := repo.GetByPublicID(ctx, id)
		if err != nil {
			administrativeRepositoryError(w, err)
			return
		}
		writeAdminJSON(w, 200, struct {
			NodeID           string                 `json:"node_id"`
			EnrollmentStatus nodes.EnrollmentStatus `json:"enrollment_status"`
			TrustStatus      nodes.TrustStatus      `json:"trust_status"`
			Version          string                 `json:"version"`
			State            *string                `json:"state"`
			LastSeenAt       *time.Time             `json:"last_seen_at"`
			Health           *nodes.BasicHealth     `json:"health,omitempty"`
		}{node.PublicID, node.EnrollmentStatus, node.TrustStatus, node.SoftwareVersion, node.CurrentState, node.LastSeenAt, node.ReportedHealth})
	}
}
