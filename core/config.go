package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"time"
)

type config struct {
	addr            string
	databaseURL     string
	logLevel        slog.Level
	shutdownTimeout time.Duration
	devAdminEnabled bool
	devAdminToken   string
	tracker         *trackerGrantIssuer
	webSeed         *webSeedConfig
	nodeLogin       *nodeLoginConfig
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{addr: "127.0.0.1:8080", logLevel: slog.LevelInfo, shutdownTimeout: 10 * time.Second}
	if value := getenv("ARKNET_CORE_ADDR"); value != "" {
		_, port, err := net.SplitHostPort(value)
		if err != nil {
			return config{}, fmt.Errorf("ARKNET_CORE_ADDR must be host:port: %w", err)
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return config{}, fmt.Errorf("ARKNET_CORE_ADDR port must be between 0 and 65535")
		}
		cfg.addr = value
	}
	if value := getenv("ARKNET_CORE_LOG_LEVEL"); value != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(value)); err != nil {
			return config{}, fmt.Errorf("ARKNET_CORE_LOG_LEVEL: %w", err)
		}
	}
	if value := getenv("ARKNET_CORE_SHUTDOWN_TIMEOUT"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return config{}, fmt.Errorf("ARKNET_CORE_SHUTDOWN_TIMEOUT must be a positive Go duration")
		}
		cfg.shutdownTimeout = duration
	}
	if value := getenv("DATABASE_URL"); value != "" {
		cfg.databaseURL = value
	} else {
		valueOrDefault := func(key, fallback string) string {
			if value := getenv(key); value != "" {
				return value
			}
			return fallback
		}
		port := valueOrDefault("PGPORT", "5432")
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return config{}, fmt.Errorf("PGPORT must be between 1 and 65535")
		}
		u := url.URL{
			Scheme: "postgresql",
			Host:   net.JoinHostPort(valueOrDefault("PGHOST", "127.0.0.1"), port),
			User:   url.UserPassword(valueOrDefault("PGUSER", "arknet"), getenv("PGPASSWORD")),
			Path:   "/" + valueOrDefault("PGDATABASE", "arknet_core"),
		}
		query := url.Values{"sslmode": {valueOrDefault("PGSSLMODE", "require")}}
		u.RawQuery = query.Encode()
		cfg.databaseURL = u.String()
	}
	flag := getenv("ARKNET_CORE_DEV_ADMIN_ENABLED")
	if flag != "" && flag != "false" && flag != "true" {
		return config{}, fmt.Errorf("ARKNET_CORE_DEV_ADMIN_ENABLED must be true or false")
	}
	cfg.devAdminEnabled = flag == "true"
	cfg.devAdminToken = getenv("ARKNET_CORE_DEV_ADMIN_TOKEN")
	if cfg.devAdminEnabled && !validDevelopmentToken(cfg.devAdminToken) {
		return config{}, fmt.Errorf("ARKNET_CORE_DEV_ADMIN_TOKEN must be exactly 64 lowercase hexadecimal characters")
	}
	if !cfg.devAdminEnabled && cfg.devAdminToken != "" {
		return config{}, fmt.Errorf("development administrator token requires explicit development administrator enablement")
	}
	tracker, err := loadTrackerGrantIssuer(getenv("ARKNET_TRACKER_ANNOUNCE_URL"), getenv("ARKNET_TRACKER_SIGNING_KEY_FILE"))
	if err != nil {
		return config{}, err
	}
	cfg.tracker = tracker
	webSeed, err := loadWebSeedConfig(getenv("ARKNET_CORE_WEB_SEED_ORIGIN"), getenv("ARKNET_CORE_WEB_SEED_ROOT"), tracker)
	if err != nil {
		return config{}, err
	}
	cfg.webSeed = webSeed
	nodeLogin, err := loadNodeLoginConfig(getenv, tracker)
	if err != nil {
		return config{}, err
	}
	cfg.nodeLogin = nodeLogin
	return cfg, nil
}
