package main

import (
	"arknet/core/internal/nodes"
	"context"
	"encoding/json"
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
			Capabilities     []nodes.Capability     `json:"capabilities"`
		}{node.PublicID, node.EnrollmentStatus, node.TrustStatus, node.SoftwareVersion, node.CurrentState, node.LastSeenAt, node.ReportedHealth,
			capabilityView(node.Capabilities)})
	}
}

func capabilityView(c []nodes.Capability) []nodes.Capability {
	if c == nil {
		return []nodes.Capability{}
	}
	return c
}

// nodeCapabilitiesHandler replaces a Node's capability set from an exact
// {"capabilities": [...]} body. An empty list leaves base routes only.
func nodeCapabilitiesHandler(repo nodes.CapabilityRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("node_id")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256))
		if !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery || err != nil {
			adminError(w, 400, "invalid request")
			return
		}
		fields, ok := exactObject(body, "capabilities")
		var values []string
		if !ok || string(fields["capabilities"]) == "null" || json.Unmarshal(fields["capabilities"], &values) != nil {
			adminError(w, 400, "body must be exactly {\"capabilities\": [...]}")
			return
		}
		capabilities, err := nodes.NormalizeCapabilities(values)
		if err != nil {
			adminError(w, 400, "capabilities must be unique values from: memberships, swarm")
			return
		}
		if repo == nil {
			adminError(w, 503, "administration unavailable")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		node, err := repo.SetCapabilities(ctx, id, capabilities)
		if err != nil {
			administrativeRepositoryError(w, err)
			return
		}
		writeAdminJSON(w, 200, struct {
			NodeID       string             `json:"node_id"`
			Capabilities []nodes.Capability `json:"capabilities"`
		}{node.PublicID, capabilityView(node.Capabilities)})
	}
}
