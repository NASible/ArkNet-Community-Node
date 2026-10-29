package main

import (
	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/release"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// version is the Agent build version; release builds may set it with
// -ldflags "-X main.version=…" (see Dockerfile ARKNET_VERSION).
var version = "0.0.1-dev"

type nodeState = localstate.State

const stateProvisioning = localstate.Provisioning
const coreConnectivityNotImplemented = "NOT_IMPLEMENTED"

type nodeStatus struct {
	NodeID                      string           `json:"node_id"`
	Version                     string           `json:"version"`
	State                       localstate.State `json:"state"`
	StateChangedAt              time.Time        `json:"state_changed_at"`
	EnrollmentState             string           `json:"enrollment_state"`
	CoreConnectivity            string           `json:"core_connectivity"`
	LastSuccessfulCoreContactAt *time.Time       `json:"last_successful_core_contact_at"`
	DatabaseHealthy             bool             `json:"database_healthy"`
	AgentHealthy                bool             `json:"agent_healthy"`
	JellyfinHealthy             *bool            `json:"jellyfin_healthy"`
	Release                     *releaseSummary  `json:"release"`
}

// releaseSummary is the current whole-stack release, or null when no valid
// manifest for this Agent build is installed.
type releaseSummary struct {
	Version string          `json:"version"`
	Channel release.Channel `json:"channel"`
}

type releaseOpt struct{ manifest *release.Manifest }

func (releaseOpt) isOption() {}

// handlerOption is the common interface for injecting optional services.
type handlerOption interface{ isOption() }

type monitorOpt struct{ *heartbeat.Monitor }

func (m monitorOpt) isOption() {}

type applicationOpt struct{ application.Adapter }

func (a applicationOpt) isOption() {}

func newHandler(repository localstate.Repository, opts ...handlerOption) http.Handler {
	var monitor *heartbeat.Monitor
	var managedApplication application.Adapter
	var media mediaOpt
	var transfers transferOpt
	var settings peerServingSettings
	var local localUsersOpt
	var global globalAuthOpt
	var misses mediaMissOpt
	var current *releaseSummary
	var updateOpt updatesOpt
	var diagnostics diagnosticsOpt
	var operatorAccounts operatorOpt
	diagnosticsEnabled := false
	for _, opt := range opts {
		switch o := opt.(type) {
		case monitorOpt:
			monitor = o.Monitor
		case applicationOpt:
			managedApplication = o.Adapter
		case mediaOpt:
			media = o
		case transferOpt:
			transfers = o
		case settingsOpt:
			settings = o.peerServingSettings
		case localUsersOpt:
			local = o
		case globalAuthOpt:
			global = o
		case mediaMissOpt:
			misses = o
		case updatesOpt:
			updateOpt = o
		case operatorOpt:
			operatorAccounts = o
		case diagnosticsOpt:
			diagnostics, diagnosticsEnabled = o, true
		case releaseOpt:
			if o.manifest != nil {
				current = &releaseSummary{Version: o.manifest.Version, Channel: o.manifest.Channel}
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		healthy := repository != nil && repository.Ping(ctx) == nil
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Healthy         bool `json:"healthy"`
			DatabaseHealthy bool `json:"database_healthy"`
		}{healthy, healthy})
	})
	mux.HandleFunc("GET /api/v1/status", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if repository == nil {
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "local state database unavailable"})
			return
		}
		connectivity := coreConnectivityNotImplemented
		if monitor != nil && monitor.Connectivity() != "" {
			connectivity = monitor.Connectivity()
		}
		status, err := buildStatus(ctx, repository, connectivity, managedApplication, current)
		if err != nil {
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "local state database unavailable"})
			return
		}
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("GET /api/v1/media", media.list)
	mux.Handle("POST /api/v1/media/{id}/selection", http.NewCrossOriginProtection().Handler(http.HandlerFunc(media.queue)))
	mux.Handle("DELETE /api/v1/media/{id}/retained", http.NewCrossOriginProtection().Handler(http.HandlerFunc(media.deleteRetained)))
	transfers.media = media
	mux.HandleFunc("GET /api/v1/transfers", transfers.list)
	mux.Handle("POST /api/v1/media/{id}/transfer/{action}", http.NewCrossOriginProtection().Handler(http.HandlerFunc(transfers.action)))
	mux.Handle("POST /api/v1/invitations", http.NewCrossOriginProtection().Handler(http.HandlerFunc(media.invite)))
	registerPeerServingSettings(mux, settings)
	registerLocalUsers(mux, local)
	registerGlobalAuth(mux, global)
	registerMediaMisses(mux, misses)
	registerUpdates(mux, updateOpt)
	registerOperators(mux, operatorAccounts)
	if diagnosticsEnabled {
		registerDiagnostics(mux, diagnosticsDeps{opt: diagnostics, repository: repository, monitor: monitor,
			jellyfin: managedApplication, media: media, transfers: transfers.service, release: current})
	}
	return mux
}

// buildStatus is the status document shared by GET /api/v1/status and the
// local `arknet-agent status` command.
func buildStatus(ctx context.Context, repository localstate.Repository, connectivity string, app application.Adapter, current *releaseSummary) (nodeStatus, error) {
	snapshot, err := repository.Snapshot(ctx)
	if err != nil {
		return nodeStatus{}, err
	}
	var lastContact *time.Time
	if !snapshot.LastSuccessfulCoreContactAt.IsZero() {
		lastContact = &snapshot.LastSuccessfulCoreContactAt
	}
	var jellyfinHealthy *bool
	if app != nil {
		h := app.Healthy(ctx)
		jellyfinHealthy = &h
	}
	return nodeStatus{
		NodeID:                      snapshot.NodeID,
		Version:                     version,
		State:                       snapshot.State,
		StateChangedAt:              snapshot.StateChangedAt,
		EnrollmentState:             snapshot.EnrollmentState,
		CoreConnectivity:            connectivity,
		LastSuccessfulCoreContactAt: lastContact,
		DatabaseHealthy:             true,
		AgentHealthy:                true,
		JellyfinHealthy:             jellyfinHealthy,
		Release:                     current,
	}, nil
}
