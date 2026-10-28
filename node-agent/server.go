package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/application/jellyfin"
	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/localusers"
	"arknet/node-agent/internal/mediamiss"
	"arknet/node-agent/internal/postgres"
	"arknet/node-agent/internal/release"
	"arknet/node-agent/internal/transfer"
	"arknet/node-agent/internal/updates"
)

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	database, err := postgres.Open(connectCtx, cfg.databaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer database.Close()
	migrationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = database.Initialize(migrationCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("database migration failed: %w", err)
	}
	logger.Info("database_ready")
	current := loadCurrentRelease(cfg.releaseManifest, logger)
	workerCtx, stopWorker := context.WithCancel(ctx)
	var monitor *heartbeat.Monitor
	if cfg.coreURL != "" {
		monitor = heartbeat.NewMonitor()
	}
	workerDone := startFederationWorker(workerCtx, cfg, database, logger, monitor)
	defer func() { stopWorker(); <-workerDone }()
	if ctx.Err() != nil {
		return nil
	}
	var jellyfinAdapter *jellyfin.Adapter
	if cfg.jellyfinURL != "" {
		jellyfinAdapter = jellyfin.New(cfg.jellyfinURL, nil)
	}
	var transferService transfer.Service
	if cfg.transferURL != "" && cfg.transferToken != "" {
		transferService = transfer.New(cfg.transferURL, cfg.transferToken)
	}
	media := mediaOpt{repository: database, allocation: cfg.mediaStorageAllocation, coreURL: cfg.coreURL, mediaDir: cfg.mediaDir}
	opts := []handlerOption{media, settingsOpt{database}, localUsersOpt{repository: database}, globalAuthOpt{repository: database, identity: media.identity}, mediaMissOpt{database}, current,
		diagnosticsOpt{coreURL: strings.TrimSuffix(cfg.coreURL, "/"), trackerOrigin: cfg.trackerOrigin, mediaDir: cfg.mediaDir}}
	var checker *updates.Checker
	if cfg.releaseIndexURL != nil {
		checker = &updates.Checker{IndexURL: cfg.releaseIndexURL, AllowHTTP: cfg.releaseIndexAllowHTTP}
	}
	updateMonitor := updates.NewMonitor(checker, current.manifest)
	opts = append(opts, updatesOpt{monitor: updateMonitor, settings: database})
	updateDone := updateMonitor.Run(workerCtx, updates.CheckInterval, func(err error) { logger.Warn("update_check_failed", "reason", updateFailureReason(err)) })
	defer func() { stopWorker(); <-updateDone }()
	if monitor != nil {
		opts = append(opts, monitorOpt{monitor})
	}
	if jellyfinAdapter != nil {
		opts = append(opts, applicationOpt{jellyfinAdapter})
	}
	if transferService != nil {
		opts = append(opts, transferOpt{service: transferService})
	}
	handler := newHandler(database, opts...)
	var reconcileDone <-chan struct{}
	if transferService != nil && jellyfinAdapter != nil && cfg.jellyfinAPIKey != "" && cfg.transferStagingDir != "" && cfg.mediaDir != "" {
		reconciler := transferReconciler{
			service: transferService, media: media, stagingDir: cfg.transferStagingDir, mediaDir: cfg.mediaDir,
			refreshJellyfin: func(callCtx context.Context) error { return jellyfinAdapter.Refresh(callCtx, cfg.jellyfinAPIKey) },
			reportReplica:   catalog.ReportReplica,
		}
		reconcileDone = runTransferReconciler(workerCtx, cfg.transferReconcileInterval, reconciler, func(error) { logger.Warn("transfer_reconciliation_failed") })
	}
	if reconcileDone != nil {
		defer func() { stopWorker(); <-reconcileDone }()
	}
	// Peer Serving: serve verified local copies only while the operator opts
	// in, the Node is CONNECTED, and Core keeps issuing seed grants.
	if seeder, ok := transferService.(transfer.Seeder); ok && cfg.coreURL != "" {
		seeding := &seedReconciler{
			seeds: seeder, media: media, peerServing: database.PeerServing, seedGrant: catalog.SeedGrant,
			nodeState: func(callCtx context.Context) (localstate.State, error) {
				snapshot, err := database.Snapshot(callCtx)
				return snapshot.State, err
			},
		}
		seedDone := runReconciler(workerCtx, cfg.transferReconcileInterval, "seed reconciliation", seeding, func(error) { logger.Warn("seed_reconciliation_failed") })
		defer func() { stopWorker(); <-seedDone }()
	}
	if cfg.coreURL != "" {
		syncer := mediaMissSyncer{media: media, repository: database, transfers: transferOpt{service: transferService, media: media}}
		missDone := runMediaMissSync(workerCtx, cfg.heartbeatInterval, syncer, func(error) { logger.Warn("media_miss_sync_failed") })
		defer func() { stopWorker(); <-missDone }()
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	return serve(ctx, server, listener, cfg.shutdownTimeout, logger)
}

func newConfiguredHandler(repository localstate.Repository, cfg config, monitor *heartbeat.Monitor) http.Handler {
	opts := make([]handlerOption, 0, 2)
	if monitor != nil {
		opts = append(opts, monitorOpt{monitor})
	}
	if cfg.jellyfinURL != "" {
		opts = append(opts, applicationOpt{jellyfin.New(cfg.jellyfinURL, nil)})
	}
	if mediaRepository, ok := repository.(mediaStorage); ok {
		media := mediaOpt{repository: mediaRepository, allocation: cfg.mediaStorageAllocation, coreURL: cfg.coreURL, mediaDir: cfg.mediaDir}
		opts = append(opts, media)
		if global, ok := repository.(globalauth.Repository); ok {
			opts = append(opts, globalAuthOpt{repository: global, identity: media.identity})
		}
	}
	if misses, ok := repository.(mediamiss.Repository); ok {
		opts = append(opts, mediaMissOpt{misses})
	}
	if settings, ok := repository.(peerServingSettings); ok {
		opts = append(opts, settingsOpt{settings})
	}
	if local, ok := repository.(localusers.Repository); ok {
		opts = append(opts, localUsersOpt{repository: local})
	}
	if cfg.transferURL != "" && cfg.transferToken != "" {
		opts = append(opts, transferOpt{service: transfer.New(cfg.transferURL, cfg.transferToken)})
	}
	return newHandler(repository, opts...)
}

// loadCurrentRelease never stops the Agent: local services keep running and
// the status API reports release null when the manifest is absent or invalid.
func loadCurrentRelease(path string, logger *slog.Logger) releaseOpt {
	if path == "" {
		logger.Warn("release_manifest_not_configured")
		return releaseOpt{}
	}
	manifest, err := release.LoadCurrent(path, version)
	switch {
	case errors.Is(err, release.ErrMismatch):
		logger.Error("release_manifest_mismatch", "agent_version", version)
		return releaseOpt{}
	case err != nil:
		logger.Error("release_manifest_invalid")
		return releaseOpt{}
	}
	logger.Info("release_manifest_loaded", "release", manifest.Version, "channel", string(manifest.Channel))
	return releaseOpt{&manifest}
}

// updateFailureReason logs a fixed category, never URLs or response content.
func updateFailureReason(err error) string {
	switch {
	case errors.Is(err, updates.ErrFetch):
		return "index_unavailable"
	case errors.Is(err, updates.ErrIndex):
		return "index_invalid"
	case errors.Is(err, updates.ErrManifest):
		return "manifest_rejected"
	}
	return "other"
}

// enrollAtStartup is a single bounded attempt, never a discovery/retry loop.
// Failure does not stop the local API or disclose Core/transport identities.
func enrollAtStartup(ctx context.Context, coreURL string, store enrollment.Storage, logger *slog.Logger) {
	if coreURL == "" {
		return
	}
	if err := enrollment.Submit(ctx, coreURL, version, store); err != nil {
		logger.Warn("enrollment_request_failed")
	}
}

func startEnrollmentWorker(ctx context.Context, coreURL string, store enrollment.Storage, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		enrollment.Run(ctx, coreURL, version, store, func(error) { logger.Warn("enrollment_request_failed") })
	}()
	return done
}

type federationStorage interface {
	enrollment.Storage
	heartbeat.Storage
}

func startFederationWorker(ctx context.Context, cfg config, store federationStorage, logger *slog.Logger, monitor *heartbeat.Monitor) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		enrollment.Run(ctx, cfg.coreURL, version, store, func(error) { logger.Warn("enrollment_request_failed") })
		heartbeat.RunWithSuccess(ctx, cfg.coreURL, version, store, cfg.heartbeatInterval, monitor,
			func(refreshCtx context.Context, identity enrollment.Identity) error {
				var refreshErrors []error
				if authorizations, ok := store.(globalauth.Repository); ok {
					if err := globalauth.Reconcile(refreshCtx, authorizations, identity); err != nil {
						refreshErrors = append(refreshErrors, err)
					}
				}
				if mediaRepository, ok := store.(mediaStorage); ok {
					objects, err := catalog.Fetch(refreshCtx, identity)
					if err == nil {
						err = mediaRepository.RetainRemovedMediaSelections(refreshCtx, objects)
					}
					if err != nil {
						refreshErrors = append(refreshErrors, err)
					}
				}
				return errors.Join(refreshErrors...)
			}, stateTransitionLogger(logger))
	}()
	return done
}

func stateTransitionLogger(logger *slog.Logger) heartbeat.TransitionObserver {
	return func(from, to localstate.State) {
		logger.Info("node_state_transition", "from_state", string(from), "to_state", string(to))
	}
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration, logger *slog.Logger) error {
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("server_started", "service", "arknet-agent", "addr", listener.Addr().String(), "version", version)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		logger.Info("shutdown_started", "timeout", timeout.String())
		// The signal context is already canceled; draining needs a fresh context.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) && err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		logger.Info("shutdown_complete")
		return nil
	}
}
