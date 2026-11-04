package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arknet.local/tracker/internal/config"
	"arknet.local/tracker/internal/server"
	trackerhttp "arknet.local/tracker/internal/tracker"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("config_error", "error", err)
		return 1
	}
	listener, err := net.Listen("tcp", cfg.BindAddr)
	if err != nil {
		logger.Error("listen_error", "error", err)
		return 1
	}
	handler := trackerhttp.NewHandler(cfg.VerifyKey, cfg.AnnounceInterval, cfg.PeerTTL, time.Now, cfg.RelayVerifyKeys...)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("tracker_started", "addr", listener.Addr().String(), "announce_interval", cfg.AnnounceInterval.String(), "peer_ttl", cfg.PeerTTL.String())
	if err := server.Run(ctx, listener, handler, cfg.ShutdownTimeout); err != nil {
		logger.Error("server_error", "error", err)
		return 1
	}
	logger.Info("shutdown_complete")
	return 0
}
