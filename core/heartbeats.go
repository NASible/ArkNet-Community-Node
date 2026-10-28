package main

import (
	"arknet/core/internal/nodes"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"
)

// exactObject rejects missing, unknown, case-folded and duplicate field names,
// including escaped spellings of duplicate names, and trailing JSON values.
func exactObject(body []byte, names ...string) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(body))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	fields := make(map[string]json.RawMessage, len(names))
	for d.More() {
		token, err = d.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || fields[name] != nil {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		fields[name] = value
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || len(fields) != len(names) {
		return nil, false
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, false
	}
	return fields, true
}
func jsonBool(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
}

func heartbeatHandler(repo nodes.HeartbeatRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			http.Error(w, "heartbeat does not accept parameters", 400)
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			http.Error(w, "JSON content type required", 415)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "heartbeat request too large", 413)
			} else {
				http.Error(w, "invalid heartbeat request", 400)
			}
			return
		}
		var request struct {
			NodeID  string `json:"node_id"`
			Version string `json:"version"`
			State   string `json:"state"`
			Health  struct {
				Healthy         bool `json:"healthy"`
				DatabaseHealthy bool `json:"database_healthy"`
			} `json:"health"`
			Capabilities struct {
				PeerServing bool `json:"peer_serving"`
			} `json:"capabilities"`
		}
		fields, valid := exactObject(body, "node_id", "version", "state", "health", "capabilities")
		health, healthValid := exactObject(fields["health"], "healthy", "database_healthy")
		capabilities, capsValid := exactObject(fields["capabilities"], "peer_serving")
		id := r.PathValue("node_id")
		if !valid || !healthValid || !capsValid || !jsonBool(health["healthy"]) || !jsonBool(health["database_healthy"]) || !jsonBool(capabilities["peer_serving"]) || json.Unmarshal(body, &request) != nil || !nodes.ValidPublicID(id) || request.NodeID != id || !nodes.ValidSoftwareVersion(request.Version) || !nodes.ValidState(request.State) {
			http.Error(w, "invalid heartbeat request", 400)
			return
		}
		if repo == nil {
			http.Error(w, "heartbeat unavailable", 503)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		node, key, err := repo.EnrollmentIdentity(ctx, id)
		if errors.Is(err, nodes.ErrNotFound) || errors.Is(err, nodes.ErrInvalidPublicID) {
			unauthorizedNode(w)
			return
		}
		if err != nil {
			http.Error(w, "heartbeat unavailable", 503)
			return
		}
		if node.PublicID != id || !verifyNodeProof(r, body, id, key) {
			unauthorizedNode(w)
			return
		}
		if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
			http.Error(w, "node is not approved", 403)
			return
		}
		proof, _ := strconv.ParseInt(r.Header.Get("X-ArkNet-Node-Timestamp"), 10, 64)
		if err := repo.AcceptHeartbeat(ctx, id, request.Version, request.State, request.Health.Healthy, request.Health.DatabaseHealthy, request.Capabilities.PeerServing, key, proof); err != nil {
			switch {
			case errors.Is(err, nodes.ErrNodeAuthentication), errors.Is(err, nodes.ErrNotFound):
				unauthorizedNode(w)
			case errors.Is(err, nodes.ErrNodeUnapproved):
				http.Error(w, "node is not approved", 403)
			case errors.Is(err, nodes.ErrHeartbeatReplay):
				http.Error(w, "heartbeat proof is not newer", 409)
			default:
				http.Error(w, "heartbeat unavailable", 503)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			NodeID   string `json:"node_id"`
			Accepted bool   `json:"accepted"`
		}{id, true})
	}
}
