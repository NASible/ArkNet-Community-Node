package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"time"
)

type peerServingSettings interface {
	PeerServing(context.Context) (bool, error)
	SetPeerServing(context.Context, bool) error
}

type settingsOpt struct{ peerServingSettings }

func (settingsOpt) isOption() {}

type peerServingResponse struct {
	Enabled bool `json:"enabled"`
}

func registerPeerServingSettings(mux *http.ServeMux, settings peerServingSettings) {
	mux.HandleFunc("GET /api/v1/settings/peer-serving", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if settings == nil {
			http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		enabled, err := settings.PeerServing(ctx)
		if err != nil {
			http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(peerServingResponse{Enabled: enabled})
	})
	mux.Handle("PUT /api/v1/settings/peer-serving", http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if settings == nil {
			http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "JSON content type required", http.StatusUnsupportedMediaType)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "settings request too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "invalid settings request", http.StatusBadRequest)
			}
			return
		}
		fields, exact := exactLocalObject(body, "enabled")
		var enabled bool
		if !exact || !jsonBoolValue(fields["enabled"]) || json.Unmarshal(fields["enabled"], &enabled) != nil {
			http.Error(w, "invalid settings request", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := settings.SetPeerServing(ctx, enabled); err != nil {
			http.Error(w, "settings unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(peerServingResponse{Enabled: enabled})
	})))
}

func exactLocalObject(body []byte, names ...string) (map[string]json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	fields := make(map[string]json.RawMessage, len(names))
	for decoder.More() {
		token, err = decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || fields[name] != nil {
			return nil, false
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		fields[name] = value
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim('}') || len(fields) != len(names) {
		return nil, false
	}
	return fields, decoder.Decode(new(any)) == io.EOF
}

func jsonBoolValue(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
}
