package main

import (
	"arknet/node-agent/internal/transfer"
	"arknet/node-agent/internal/updates"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type config struct {
	transferURL               string
	transferToken             string
	mediaStorageAllocation    int64
	heartbeatInterval         time.Duration
	coreURL                   string
	addr                      string
	memberAddr                string
	databaseURL               string
	logLevel                  slog.Level
	shutdownTimeout           time.Duration
	jellyfinURL               string
	jellyfinAPIKey            string
	transferStagingDir        string
	mediaDir                  string
	transferReconcileInterval time.Duration
	releaseManifest           string
	releaseIndexURL           *url.URL
	releaseIndexAllowHTTP     bool
	trackerOrigin             string
	globalLogin               *globalLoginConfig
}

func loadConfig(getenv func(string) string) (config, error) {
	interval := 30 * time.Second
	if value := getenv("ARKNET_HEARTBEAT_INTERVAL"); value != "" {
		var err error
		interval, err = time.ParseDuration(value)
		if err != nil || interval < 5*time.Second || interval > 5*time.Minute {
			return config{}, fmt.Errorf("ARKNET_HEARTBEAT_INTERVAL must be a Go duration between 5s and 5m")
		}
	}
	cfg := config{heartbeatInterval: interval, addr: "127.0.0.1:8081", memberAddr: "127.0.0.1:8082", logLevel: slog.LevelInfo, shutdownTimeout: 10 * time.Second, transferReconcileInterval: 30 * time.Second}
	allocationGiB := getenv("ARKNET_MEDIA_STORAGE_ALLOCATION_GIB")
	allocationBytes := getenv("ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES")
	if allocationGiB != "" && allocationBytes != "" {
		return config{}, fmt.Errorf("set only ARKNET_MEDIA_STORAGE_ALLOCATION_GIB; do not combine it with legacy ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES")
	}
	if allocationGiB != "" {
		const bytesPerGiB int64 = 1 << 30
		allocation, err := strconv.ParseInt(allocationGiB, 10, 64)
		if err != nil || allocation < -1 || allocation > math.MaxInt64/bytesPerGiB {
			return config{}, fmt.Errorf("ARKNET_MEDIA_STORAGE_ALLOCATION_GIB must be a whole GiB count between 0 and %d, or -1 for unlimited", math.MaxInt64/bytesPerGiB)
		}
		cfg.mediaStorageAllocation = allocation
		if allocation != -1 {
			cfg.mediaStorageAllocation *= bytesPerGiB
		}
	} else if allocationBytes != "" {
		// Preserve existing deployments without silently reinterpreting bytes as GiB.
		allocation, err := strconv.ParseInt(allocationBytes, 10, 64)
		if err != nil || allocation < -1 {
			return config{}, fmt.Errorf("ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES must be a non-negative integer byte count, or -1 for unlimited")
		}
		cfg.mediaStorageAllocation = allocation
	}
	for _, binding := range []struct {
		key    string
		target *string
	}{{"ARKNET_AGENT_ADDR", &cfg.addr}, {"ARKNET_MEMBER_ADDR", &cfg.memberAddr}} {
		if value := getenv(binding.key); value != "" {
			_, port, err := net.SplitHostPort(value)
			if err != nil {
				return config{}, fmt.Errorf("%s must be host:port: %w", binding.key, err)
			}
			n, err := strconv.Atoi(port)
			if err != nil || n < 0 || n > 65535 {
				return config{}, fmt.Errorf("%s port must be between 0 and 65535", binding.key)
			}
			*binding.target = value
		}
	}
	if value := getenv("ARKNET_AGENT_LOG_LEVEL"); value != "" {
		if err := cfg.logLevel.UnmarshalText([]byte(value)); err != nil {
			return config{}, fmt.Errorf("ARKNET_AGENT_LOG_LEVEL: %w", err)
		}
	}
	if value := getenv("ARKNET_AGENT_SHUTDOWN_TIMEOUT"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			return config{}, fmt.Errorf("ARKNET_AGENT_SHUTDOWN_TIMEOUT must be a positive Go duration")
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
			Path:   "/" + valueOrDefault("PGDATABASE", "arknet_node"),
		}
		query := url.Values{"sslmode": {valueOrDefault("PGSSLMODE", "require")}}
		u.RawQuery = query.Encode()
		cfg.databaseURL = u.String()
	}
	cfg.coreURL = getenv("ARKNET_CORE_URL")
	if cfg.coreURL != "" && !validCoreBaseURL(cfg.coreURL, getenv("ARKNET_CORE_ALLOW_HTTP") == "true") {
		return config{}, fmt.Errorf("ARKNET_CORE_URL must be a valid HTTPS base URL (HTTP requires loopback or explicit development opt-in)")
	}
	globalLogin, err := loadGlobalLoginConfig(getenv, cfg.coreURL)
	if err != nil {
		return config{}, err
	}
	cfg.globalLogin = globalLogin
	if value := getenv("JELLYFIN_API_URL"); value != "" {
		if _, err := url.ParseRequestURI(value); err != nil {
			return config{}, fmt.Errorf("JELLYFIN_API_URL must be a valid URL")
		}
		cfg.jellyfinURL = value
	}
	cfg.jellyfinAPIKey = getenv("JELLYFIN_API_KEY")
	if strings.ContainsAny(cfg.jellyfinAPIKey, "\r\n\x00") || len(cfg.jellyfinAPIKey) > 512 {
		return config{}, fmt.Errorf("JELLYFIN_API_KEY is invalid")
	}
	cfg.transferStagingDir = getenv("ARKNET_TRANSFER_STAGING_DIR")
	cfg.mediaDir = getenv("ARKNET_MEDIA_DIR")
	for name, value := range map[string]string{"ARKNET_TRANSFER_STAGING_DIR": cfg.transferStagingDir, "ARKNET_MEDIA_DIR": cfg.mediaDir} {
		if value != "" && (!filepath.IsAbs(value) || filepath.Clean(value) != value) {
			return config{}, fmt.Errorf("%s must be a clean absolute path", name)
		}
	}
	if value := getenv("ARKNET_TRANSFER_RECONCILE_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration < 5*time.Second || duration > 5*time.Minute {
			return config{}, fmt.Errorf("ARKNET_TRANSFER_RECONCILE_INTERVAL must be between 5s and 5m")
		}
		cfg.transferReconcileInterval = duration
	}
	if value := getenv("ARKNET_RELEASE_MANIFEST"); value != "" {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return config{}, fmt.Errorf("ARKNET_RELEASE_MANIFEST must be a clean absolute path")
		}
		cfg.releaseManifest = value
	}
	if value := getenv("ARKNET_RELEASE_INDEX_URL"); value != "" {
		cfg.releaseIndexAllowHTTP = getenv("ARKNET_RELEASE_INDEX_ALLOW_HTTP") == "true"
		u, err := url.Parse(value)
		if err != nil || updates.ValidateURL(u, cfg.releaseIndexAllowHTTP) != nil || len(value) > 2048 {
			return config{}, fmt.Errorf("ARKNET_RELEASE_INDEX_URL must be an HTTPS URL (HTTP requires loopback or ARKNET_RELEASE_INDEX_ALLOW_HTTP=true)")
		}
		cfg.releaseIndexURL = u
	}
	if value := getenv("ARKNET_TRANSFER_TRACKER_ORIGIN"); value != "" {
		u, err := url.Parse(value)
		if err != nil || updates.ValidateURL(u, getenv("ARKNET_TRANSFER_TRACKER_ALLOW_HTTP") == "true") != nil ||
			(u.EscapedPath() != "" && u.EscapedPath() != "/") || u.RawQuery != "" {
			return config{}, fmt.Errorf("ARKNET_TRANSFER_TRACKER_ORIGIN must be an HTTPS origin (HTTP requires loopback or ARKNET_TRANSFER_TRACKER_ALLOW_HTTP=true)")
		}
		cfg.trackerOrigin = strings.TrimSuffix(value, "/")
	}
	cfg.transferURL = getenv("ARKNET_TRANSFER_URL")
	cfg.transferToken = getenv("ARKNET_TRANSFER_TOKEN")
	if cfg.transferURL != "" || cfg.transferToken != "" {
		if err := transfer.ValidateConfig(cfg.transferURL, cfg.transferToken); err != nil {
			return config{}, fmt.Errorf("ARKNET_TRANSFER_URL and ARKNET_TRANSFER_TOKEN must configure a valid local transfer endpoint and service token")
		}
	}
	return cfg, nil
}

// validCoreBaseURL accepts an HTTPS origin, or plain HTTP for loopback or with
// the explicit development opt-in.
func validCoreBaseURL(raw string, allowHTTP bool) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	invalid := u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || strings.HasSuffix(u.Host, ":") || (u.EscapedPath() != "" && u.EscapedPath() != "/")
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
		invalid = invalid || u.Scheme != "http" || (!loopback && !allowHTTP)
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		invalid = invalid || err != nil || port < 1 || port > 65535
	}
	return !invalid
}
