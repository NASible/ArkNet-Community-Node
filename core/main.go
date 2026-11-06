package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"arknet/core/internal/postgres"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "admin" {
		os.Exit(adminMain(os.Args[2:]))
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("configuration_failed", "error", err.Error())
		os.Exit(1)
	}
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.logLevel}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("server_failed", "error", err.Error())
		os.Exit(1)
	}
}

// adminMain runs `arknet-core admin ...` against Core's configured database.
// Stdout carries only the command's JSON result; diagnostics go to stderr.
func adminMain(args []string) int {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration failed:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := postgres.Open(ctx, cfg.databaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "database unavailable")
		return 1
	}
	defer database.Close()
	if err := database.Migrate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "database migration failed")
		return 1
	}
	return runAdminCommand(ctx, args, database, os.Stdout, os.Stderr)
}
