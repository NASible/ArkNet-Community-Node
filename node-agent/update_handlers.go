package main

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"arknet/node-agent/internal/release"
	"arknet/node-agent/internal/updates"
)

type automaticUpdateSettings interface {
	AutomaticUpdates(context.Context) (bool, error)
	SetAutomaticUpdates(context.Context, bool) error
}

type updatesOpt struct {
	monitor  *updates.Monitor
	settings automaticUpdateSettings
}

func (updatesOpt) isOption() {}

type updateDiscovery struct {
	State     updates.State `json:"state"`
	CheckedAt *time.Time    `json:"checked_at"`
}

type updatesResponse struct {
	CurrentVersion   *string            `json:"current_version"`
	Channel          *release.Channel   `json:"channel"`
	Available        *updates.Available `json:"available"`
	AutomaticUpdates bool               `json:"automatic_updates"`
	// Automatic installation is not implemented yet: discovery only.
	AutomaticInstall bool            `json:"automatic_install_supported"`
	Discovery        updateDiscovery `json:"discovery"`
}

func (o updatesOpt) view(ctx context.Context, status updates.Status) (updatesResponse, error) {
	enabled, err := o.settings.AutomaticUpdates(ctx)
	if err != nil {
		return updatesResponse{}, err
	}
	response := updatesResponse{AutomaticUpdates: enabled, Available: status.Available, Discovery: updateDiscovery{State: status.State}}
	if current := o.monitor.Current(); current != nil {
		response.CurrentVersion, response.Channel = &current.Version, &current.Channel
	}
	if !status.CheckedAt.IsZero() {
		response.Discovery.CheckedAt = &status.CheckedAt
	}
	return response, nil
}

func writeUpdates(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// readEmptyJSON accepts exactly `{}` with a JSON content type.
func readEmptyJSON(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeUpdates(w, http.StatusUnsupportedMediaType, map[string]string{"error": "json_required"})
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64))
	if err != nil {
		writeUpdates(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return false
	}
	if _, exact := exactLocalObject(body); !exact {
		writeUpdates(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return false
	}
	return true
}

func registerUpdates(mux *http.ServeMux, o updatesOpt) {
	unavailable := func(w http.ResponseWriter) {
		writeUpdates(w, http.StatusServiceUnavailable, map[string]string{"error": "updates_unavailable"})
	}
	mux.HandleFunc("GET /api/v1/updates", func(w http.ResponseWriter, r *http.Request) {
		if o.monitor == nil || o.settings == nil {
			unavailable(w)
			return
		}
		if r.URL.RawQuery != "" {
			writeUpdates(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		view, err := o.view(ctx, o.monitor.Status())
		if err != nil {
			unavailable(w)
			return
		}
		writeUpdates(w, http.StatusOK, view)
	})
	// "Check now": read-only discovery, never installs anything.
	mux.Handle("POST /api/v1/updates/check", http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.monitor == nil || o.settings == nil {
			unavailable(w)
			return
		}
		if r.URL.RawQuery != "" {
			writeUpdates(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		if !readEmptyJSON(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*updates.RequestTimeout+2*time.Second)
		defer cancel()
		// A failed check is reported in the body as CHECK_FAILED, not as an
		// HTTP error: the Agent itself is healthy.
		status, _ := o.monitor.Check(ctx, true)
		view, err := o.view(ctx, status)
		if err != nil {
			unavailable(w)
			return
		}
		writeUpdates(w, http.StatusOK, view)
	})))
	mux.HandleFunc("GET /api/v1/settings/automatic-updates", func(w http.ResponseWriter, r *http.Request) {
		if o.settings == nil {
			unavailable(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		enabled, err := o.settings.AutomaticUpdates(ctx)
		if err != nil {
			unavailable(w)
			return
		}
		writeUpdates(w, http.StatusOK, map[string]bool{"enabled": enabled})
	})
	mux.Handle("PUT /api/v1/settings/automatic-updates", http.NewCrossOriginProtection().Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.settings == nil {
			unavailable(w)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeUpdates(w, http.StatusUnsupportedMediaType, map[string]string{"error": "json_required"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256))
		fields, exact := exactLocalObject(body, "enabled")
		var enabled bool
		if err != nil || r.URL.RawQuery != "" || !exact || !jsonBoolValue(fields["enabled"]) || json.Unmarshal(fields["enabled"], &enabled) != nil {
			writeUpdates(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := o.settings.SetAutomaticUpdates(ctx, enabled); err != nil {
			unavailable(w)
			return
		}
		writeUpdates(w, http.StatusOK, map[string]bool{"enabled": enabled})
	})))
}
