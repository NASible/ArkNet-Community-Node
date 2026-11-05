package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const version = "0.0.1-dev"

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}
func failure(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func newHandler(token string, e transferEngine) http.Handler {
	expected := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") {
			values := r.Header.Values("Authorization")
			actual := sha256.Sum256([]byte(r.Header.Get("Authorization")))
			if len(values) != 1 || subtle.ConstantTimeCompare(actual[:], expected[:]) != 1 {
				failure(w, 401, "unauthorized")
				return
			}
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, maxRelayRequestBody+1))
		if err != nil || len(data) > maxRelayRequestBody || (r.Method == http.MethodGet && len(data) != 0) {
			failure(w, 400, "invalid_request")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.EscapedPath() != r.URL.Path {
			failure(w, 400, "invalid_request")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/transfer/v1/transfers") {
			transferRoute(w, r, e)
			return
		}
		if r.URL.Path == seedsPrefix || strings.HasPrefix(r.URL.Path, seedsPrefix+"/") {
			seedRoute(w, r, e)
			return
		}
		switch r.URL.Path {
		case "/health", "/api/transfer/v1/version":
			if r.Method != http.MethodGet {
				failure(w, 405, "method_not_allowed")
				return
			}
			if r.URL.Path == "/health" {
				writeJSON(w, 200, map[string]any{"healthy": true, "mock": false, "backend": "libtorrent"})
			} else {
				writeJSON(w, 200, map[string]any{"version": version, "mock": false, "backend": "libtorrent", "policy": map[string]bool{"v2_only": true, "dht": false, "pex": false, "lsd": false}})
			}
		default:
			failure(w, 404, "not_found")
		}
	})
}
