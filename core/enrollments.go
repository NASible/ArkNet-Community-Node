package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/core/internal/nodes"
)

const enrollmentBodyLimit = 4096

func enrollmentHandler(repository nodes.EnrollmentRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			http.Error(w, "JSON content type required", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, enrollmentBodyLimit)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "enrollment request too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "invalid enrollment request", http.StatusBadRequest)
			}
			return
		}
		var request struct {
			NodeID       string                     `json:"node_id"`
			Version      string                     `json:"version"`
			Capabilities map[string]json.RawMessage `json:"capabilities"`
			PublicKey    string                     `json:"public_key"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid enrollment request", http.StatusBadRequest)
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			http.Error(w, "invalid enrollment request", http.StatusBadRequest)
			return
		}
		// encoding/json accepts case-insensitive struct fields. Require exact wire keys.
		var fields map[string]json.RawMessage
		if !uniqueEnrollmentFields(body) || json.Unmarshal(body, &fields) != nil || len(fields) != 4 || fields["public_key"] == nil || fields["node_id"] == nil || fields["version"] == nil || fields["capabilities"] == nil || request.Capabilities == nil || len(request.Capabilities) != 0 || !nodes.ValidPublicID(request.NodeID) || !nodes.ValidSoftwareVersion(request.Version) {
			http.Error(w, "invalid enrollment request", http.StatusBadRequest)
			return
		}
		if fields["public_key"] != nil {
			key, err := base64.StdEncoding.Strict().DecodeString(request.PublicKey)
			if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != request.PublicKey {
				http.Error(w, "invalid enrollment request", http.StatusBadRequest)
				return
			}
			if !verifyNodeProof(r, body, request.NodeID, key) {
				unauthorizedNode(w)
				return
			}
			handshake, ok := repository.(nodes.HandshakeRepository)
			if !ok {
				http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			_, err = handshake.RegisterEnrollment(ctx, request.NodeID, request.Version, key)
			if err != nil && !errors.Is(err, nodes.ErrEnrollmentConflict) {
				http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(struct {
				NodeID           string                 `json:"node_id"`
				EnrollmentStatus nodes.EnrollmentStatus `json:"enrollment_status"`
			}{request.NodeID, nodes.EnrollmentPending})
			return
		}
	}
}

// Decode top-level keys separately because encoding/json silently accepts
// duplicates. Only exact, unique protocol field names are allowed.
func uniqueEnrollmentFields(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := make(map[string]bool)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		name, ok := token.(string)
		if !ok || seen[name] || (name != "node_id" && name != "version" && name != "capabilities" && name != "public_key") {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return false
		}
	}
	token, err = decoder.Token()
	return err == nil && token == json.Delim('}')
}
