package main

import (
	"crypto/ed25519"
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
	tracker         *trackerGrantIssuer
	webSeed         *webSeedConfig
	nodeLogin       *nodeLoginConfig
	relays          *relaysConfig
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
	for _, removed := range []string{"ARKNET_CORE_DEV_ADMIN_ENABLED", "ARKNET_CORE_DEV_ADMIN_TOKEN"} {
		if getenv(removed) != "" {
			return config{}, fmt.Errorf("%s was removed; mint a scoped administrator token with `arknet-core admin create-token`", removed)
		}
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
	var assertionKeys []ed25519.PrivateKey
	if nodeLogin != nil {
		assertionKeys = append(assertionKeys, nodeLogin.signer.PrivateKey())
	}
	relays, err := loadRelaysConfig(getenv, tracker, assertionKeys...)
	if err != nil {
		return config{}, err
	}
	cfg.relays = relays
	return cfg, nil
}
