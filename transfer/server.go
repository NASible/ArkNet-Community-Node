package main

import (
	"arknet/transfer/backend"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

func newServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError)}
}
func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	worker, err := backend.Start(ctx, backend.Config{
		Executable: cfg.workerPath, TrackerOrigin: cfg.trackerOrigin,
		TrackerAllowHTTP: cfg.trackerAllowHTTP, StagingDir: cfg.stagingDir,
		WebSeedOrigin: cfg.webSeedOrigin, WebSeedAllowHTTP: cfg.webSeedAllowHTTP,
		StateDir: cfg.stateDir, ListenInterfaces: cfg.listenInterfaces,
		DownloadRateLimit: cfg.downloadRateLimit,
	})
	if err != nil {
		return fmt.Errorf("start transfer worker: %w", err)
	}
	handler := newHandler(cfg.token, worker)
	serveErr := serve(ctx, newServer(handler, logger), listener, 5*time.Second, logger)
	closeErr := worker.Close()
	if serveErr != nil {
		return serveErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}
func serve(ctx context.Context, s *http.Server, listener net.Listener, timeout time.Duration, logger *slog.Logger) error {
	defer s.Close()
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	logger.Info("server_started", "service", "arknet-transfer", "addr", listener.Addr().String(), "version", version, "mock", true)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		logger.Info("shutdown_started")
		drain, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := s.Shutdown(drain); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		logger.Info("shutdown_complete")
		return nil
	}
}
