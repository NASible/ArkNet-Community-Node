package main

import (
	"arknet/core/internal/nodes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// This verifies identity and signed request scope within a bounded time window.
// Read-only status and idempotent registration need no mutation replay ledger.
// Heartbeat mutation MUST additionally atomically persist a newer proof timestamp;
// this proof alone is not a session token or general mutation authority.
func verifyNodeProof(r *http.Request, body []byte, id string, key []byte) bool {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || len(key) != ed25519.PublicKeySize {
		return false
	}
	timestamps := r.Header.Values("X-ArkNet-Node-Timestamp")
	signatures := r.Header.Values("X-ArkNet-Node-Signature")
	if len(timestamps) != 1 || len(signatures) != 1 {
		return false
	}
	ts, err := strconv.ParseInt(timestamps[0], 10, 64)
	if err != nil || strconv.FormatInt(ts, 10) != timestamps[0] {
		return false
	}
	now := time.Now().Unix()
	if ts < now-60 || ts > now+60 {
		return false
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(signatures[0])
	if err != nil || len(sig) != ed25519.SignatureSize || base64.StdEncoding.EncodeToString(sig) != signatures[0] {
		return false
	}
	message := fmt.Sprintf("ARKNET-NODE-V1\n%s\n%s\n%x\n%s\n%s", r.Method, r.URL.EscapedPath(), sha256.Sum256(body), timestamps[0], id)
	return ed25519.Verify(ed25519.PublicKey(key), []byte(message), sig)
}
func unauthorizedNode(w http.ResponseWriter) {
	http.Error(w, "node authentication failed", http.StatusUnauthorized)
}
func writeNodeStatus(w http.ResponseWriter, node nodes.Node) {
	valid := node.EnrollmentStatus == nodes.EnrollmentPending && node.TrustStatus == nodes.TrustUntrusted || node.EnrollmentStatus == nodes.EnrollmentApproved && node.TrustStatus == nodes.TrustTrusted || node.EnrollmentStatus == nodes.EnrollmentRejected && node.TrustStatus == nodes.TrustBlocked || node.EnrollmentStatus == nodes.EnrollmentRevoked && node.TrustStatus == nodes.TrustBlocked
	if !valid {
		http.Error(w, "enrollment unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		NodeID           string                 `json:"node_id"`
		EnrollmentStatus nodes.EnrollmentStatus `json:"enrollment_status"`
		TrustStatus      nodes.TrustStatus      `json:"trust_status"`
	}{node.PublicID, node.EnrollmentStatus, node.TrustStatus})
}
func enrollmentStatusHandler(repo nodes.HandshakeRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		id := r.PathValue("node_id")
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
		if r.Method != http.MethodGet || err != nil || len(body) != 0 || !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
			unauthorizedNode(w)
			return
		}
		if repo == nil {
			http.Error(w, "enrollment unavailable", 503)
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
			http.Error(w, "enrollment unavailable", 503)
			return
		}
		if node.PublicID != id || !verifyNodeProof(r, nil, id, key) {
			unauthorizedNode(w)
			return
		}
		writeNodeStatus(w, node)
	}
}
