package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"arknet/core/internal/postgres"
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
	err = database.Migrate(migrationCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("database migration failed: %w", err)
	}
	logger.Info("database_ready")
	var authorizer adminAuthorizer
	if cfg.devAdminEnabled {
		authorizer = developmentAdminAuthorizer(cfg.devAdminToken)
		logger.Warn("development_administrative_authorization_enabled")
	}
	if cfg.nodeLogin != nil {
		cfg.nodeLogin.logger = logger
		logger.Info("node_login_enabled", "key_id", cfg.nodeLogin.signer.KeyID().String(), "published_keys", len(cfg.nodeLogin.published))
	}
	server := &http.Server{
		Handler:           withAdministration(newCoreHandler(database, cfg.tracker, cfg.webSeed, cfg.nodeLogin, database), database, authorizer),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	return serve(ctx, server, listener, cfg.shutdownTimeout, logger)
}

func serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration, logger *slog.Logger) error {
	defer server.Close()
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.Info("server_started", "service", "arknet-core", "addr", listener.Addr().String(), "version", version)
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
