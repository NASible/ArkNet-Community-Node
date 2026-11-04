// Package control serves the relay's HTTP control listener: a health check
// and the Core-signed revocation endpoint. The signature is the
// authentication; no other route exists.
package control

import (
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"arknet.local/relay/internal/revocation"
)

const RevocationsPath = "/api/relay/v1/revocations"

type Revoker interface {
	Revoke(subjects [][16]byte) int
}

func NewHandler(revoker Revoker, key ed25519.PublicKey, relayID string, now func() time.Time, logger *slog.Logger) http.Handler {
	guard := revocation.NewReplayGuard()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST "+RevocationsPath, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, revocation.MaxTokenBytes))
		if err != nil {
			logger.Warn("relay_revocation_rejected", "reason", "too_large")
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		current := now()
		rev, err := revocation.Verify(strings.TrimSpace(string(body)), key, relayID, current)
		if err != nil {
			logger.Warn("relay_revocation_rejected", "reason", reason(err))
			http.Error(w, "invalid revocation", http.StatusUnauthorized)
			return
		}
		if err := guard.Accept(rev, current); err != nil {
			status := http.StatusConflict
			if errors.Is(err, revocation.ErrBusy) {
				status = http.StatusServiceUnavailable
			}
			logger.Warn("relay_revocation_rejected", "reason", reason(err))
			http.Error(w, "revocation not applied", status)
			return
		}
		closed := revoker.Revoke(rev.Subjects)
		logger.Info("relay_revocation_received", "subjects", len(rev.Subjects), "connections_closed", closed)
		w.WriteHeader(http.StatusNoContent)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func reason(err error) string {
	switch {
	case errors.Is(err, revocation.ErrAudience):
		return "audience"
	case errors.Is(err, revocation.ErrStale):
		return "stale"
	case errors.Is(err, revocation.ErrReplay):
		return "replay"
	case errors.Is(err, revocation.ErrBusy):
		return "busy"
	default:
		return "invalid"
	}
}
