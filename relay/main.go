// Command arknet-relay is ArkNet's private peer relay (Prompt 48A,
// docs/private-relay-design.md). It splices Node-to-Node streams authorized
// by Core-signed tracker grants, and cuts sessions on Core-signed revocations.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arknet.local/relay/internal/config"
	"arknet.local/relay/internal/control"
	"arknet.local/relay/internal/relay"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("config_error", "error", err)
		return 1
	}
	relayListener, err := tls.Listen("tcp", cfg.BindAddr, relay.TLSConfig(cfg.Certificate))
	if err != nil {
		logger.Error("listen_error", "listener", "relay")
		return 1
	}
	controlListener, err := net.Listen("tcp", cfg.ControlBindAddr)
	if err != nil {
		relayListener.Close()
		logger.Error("listen_error", "listener", "control")
		return 1
	}
	r := relay.New(relay.Config{GrantKey: cfg.GrantKey, MaxGrantLifetime: cfg.MaxGrantLifetime,
		MaxConnections: cfg.MaxConnections, HeaderTimeout: 10 * time.Second, AcceptTimeout: 10 * time.Second,
		SweepInterval: time.Second, Now: time.Now, Logger: logger})
	controlServer := &http.Server{
		Handler:           control.NewHandler(r, cfg.RevocationKey, cfg.RelayID, time.Now, logger),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	relayDone := make(chan error, 1)
	go func() { relayDone <- r.Serve(ctx, relayListener) }()
	controlDone := make(chan error, 1)
	go func() { controlDone <- controlServer.Serve(controlListener) }()
	logger.Info("relay_started", "addr", relayListener.Addr().String(), "control_addr", controlListener.Addr().String(),
		"max_connections", cfg.MaxConnections, "max_grant_lifetime", cfg.MaxGrantLifetime.String())

	status := 0
	select {
	case <-ctx.Done():
	case err := <-relayDone:
		logger.Error("server_error", "listener", "relay", "error", err)
		status = 1
	case err := <-controlDone:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server_error", "listener", "control")
			status = 1
		}
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := controlServer.Shutdown(shutdownCtx); err != nil {
		_ = controlServer.Close()
	}
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		status = 1
	}
	logger.Info("shutdown_complete")
	return status
}
