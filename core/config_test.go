package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTrackerSigningKey(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tracker.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(privateKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:8080" || cfg.logLevel != slog.LevelInfo || cfg.shutdownTimeout != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := map[string]string{
		"ARKNET_CORE_ADDR":             ":9000",
		"ARKNET_CORE_LOG_LEVEL":        "debug",
		"ARKNET_CORE_SHUTDOWN_TIMEOUT": "2s",
	}
	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != ":9000" || cfg.logLevel != slog.LevelDebug || cfg.shutdownTimeout != 2*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigEnablesTrackerGrantsWhenURLAndKeyAreConfigured(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	env := map[string]string{
		"ARKNET_TRACKER_ANNOUNCE_URL":     "https://tracker.example.test:8443",
		"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath,
	}
	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tracker == nil || cfg.tracker.announceBase.String() != env["ARKNET_TRACKER_ANNOUNCE_URL"] {
		t.Fatalf("tracker configuration not loaded: %+v", cfg.tracker)
	}
}

func TestLoadConfigRejectsUnsafeOrAmbiguousTrackerURLs(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	for _, announceURL := range []string{
		"http://tracker.example.test",
		"http://tracker",
		"http://192.168.1.10:6969",
		"http://tracker.internal:6969",
		"ftp://tracker.example.test",
		"https://user:pass@tracker.example.test",
		"https://tracker.example.test/base",
		"https://tracker.example.test?query=value",
		"https://tracker.example.test#fragment",
		"https:///missing-host",
		"https://tracker.example.test:",
		"https://tracker.example.test:0",
		"https://tracker.example.test:65536",
		"https://tracker.example.test:abc",
		"https://tracker.example.test:0x50",
		`https://tracker.example.test\@evil.example`,
		"https://tracker%2eexample.test",
		"https://[fe80::1%25en0]:6969",
		"https://tracker。example.test",
	} {
		t.Run(announceURL, func(t *testing.T) {
			env := map[string]string{
				"ARKNET_TRACKER_ANNOUNCE_URL":     announceURL,
				"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath,
			}
			if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
				t.Fatal("expected tracker URL configuration error")
			}
		})
	}
}

func TestLoadConfigAllowsHTTPOnlyForLoopbackAndTrackerSidecar(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	for _, announceURL := range []string{
		"http://127.0.0.1:6969",
		"http://[::1]:6969",
		"http://localhost:6969",
		"http://arknet-tracker:6969",
		"http://ARKNET-TRACKER:6969",
	} {
		t.Run(announceURL, func(t *testing.T) {
			env := map[string]string{
				"ARKNET_TRACKER_ANNOUNCE_URL":     announceURL,
				"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath,
			}
			if _, err := loadConfig(func(key string) string { return env[key] }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadConfigRejectsPartialTrackerConfiguration(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	for _, env := range []map[string]string{
		{"ARKNET_TRACKER_ANNOUNCE_URL": "https://tracker.example.test"},
		{"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath},
	} {
		if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
			t.Fatal("expected partial tracker configuration to fail")
		}
	}
}

func TestLoadConfigStrictlyDecodesTrackerSigningKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tracker.key")
	encoded := base64.StdEncoding.EncodeToString(privateKey)
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ARKNET_TRACKER_ANNOUNCE_URL":     "https://tracker.example.test",
		"ARKNET_TRACKER_SIGNING_KEY_FILE": path,
	}
	if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
		t.Fatal("expected surrounding whitespace in key file to fail strict decoding")
	}
}

func TestLoadConfigRejectsInconsistentTrackerPrivateKey(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateKey[ed25519.PrivateKeySize-1] ^= 0xff
	path := filepath.Join(t.TempDir(), "tracker.key")
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(privateKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"ARKNET_TRACKER_ANNOUNCE_URL":     "https://tracker.example.test",
		"ARKNET_TRACKER_SIGNING_KEY_FILE": path,
	}
	if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
		t.Fatal("expected inconsistent Ed25519 private key to fail")
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"ARKNET_CORE_ADDR", "missing-port"},
		{"ARKNET_CORE_ADDR", "localhost:bad"},
		{"ARKNET_CORE_ADDR", ":65536"},
		{"ARKNET_CORE_LOG_LEVEL", "verbose"},
		{"ARKNET_CORE_SHUTDOWN_TIMEOUT", "not-a-duration"},
		{"ARKNET_CORE_SHUTDOWN_TIMEOUT", "0s"},
		{"ARKNET_CORE_SHUTDOWN_TIMEOUT", "-1s"},
	} {
		t.Run(tt.key+"/"+tt.value, func(t *testing.T) {
			_, err := loadConfig(func(key string) string {
				if key == tt.key {
					return tt.value
				}
				return ""
			})
			if err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestDatabaseConfigFromPostgresEnvironment(t *testing.T) {
	env := map[string]string{"PGHOST": "postgres", "PGPORT": "5432", "PGDATABASE": "arknet_core", "PGUSER": "arknet", "PGPASSWORD": "special:@/?# password", "PGSSLMODE": "disable"}
	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.Host != "postgres:5432" || u.Path != "/arknet_core" || u.User.Username() != "arknet" || password != env["PGPASSWORD"] || u.Query().Get("sslmode") != "disable" {
		t.Fatal("database configuration mismatch")
	}
}

func TestDatabaseURLOverridesPostgresEnvironment(t *testing.T) {
	want := "postgresql://example.invalid/db?sslmode=verify-full"
	cfg, err := loadConfig(func(key string) string {
		if key == "DATABASE_URL" {
			return want
		}
		return ""
	})
	if err != nil || cfg.databaseURL != want {
		t.Fatal("DATABASE_URL override not honored")
	}
}

func TestDatabaseConfigDefaultsToTLS(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.databaseURL)
	if err != nil || u.Query().Get("sslmode") != "require" {
		t.Fatal("database defaults must require TLS")
	}
}

func TestInvalidPostgresPortDoesNotLeakCredentials(t *testing.T) {
	for _, port := range []string{"bad", "0", "65536"} {
		_, err := loadConfig(func(key string) string {
			if key == "PGPORT" {
				return port
			}
			if key == "PGPASSWORD" {
				return "SECRET"
			}
			return ""
		})
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("expected safe configuration error")
		}
	}
}

func TestLoadConfigEnablesWebSeedOnlyWithPairedStrictConfigurationAndTrackerKey(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	root := t.TempDir()
	env := map[string]string{
		"ARKNET_TRACKER_ANNOUNCE_URL":     "https://tracker.example.test",
		"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath,
		"ARKNET_CORE_WEB_SEED_ORIGIN":     "https://core.example.test:8443",
		"ARKNET_CORE_WEB_SEED_ROOT":       root,
	}
	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.webSeed == nil || cfg.webSeed.origin.String() != env["ARKNET_CORE_WEB_SEED_ORIGIN"] || cfg.webSeed.root != root {
		t.Fatalf("web seed configuration not loaded: %+v", cfg.webSeed)
	}
}

func TestLoadConfigRejectsPartialUnsafeOrUnbackedWebSeedConfiguration(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	root := t.TempDir()
	base := map[string]string{
		"ARKNET_TRACKER_ANNOUNCE_URL":     "https://tracker.example.test",
		"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath,
		"ARKNET_CORE_WEB_SEED_ORIGIN":     "https://core.example.test",
		"ARKNET_CORE_WEB_SEED_ROOT":       root,
	}
	tests := []struct {
		name   string
		change func(map[string]string)
	}{
		{"origin only", func(env map[string]string) { delete(env, "ARKNET_CORE_WEB_SEED_ROOT") }},
		{"root only", func(env map[string]string) { delete(env, "ARKNET_CORE_WEB_SEED_ORIGIN") }},
		{"without tracker issuer", func(env map[string]string) {
			delete(env, "ARKNET_TRACKER_ANNOUNCE_URL")
			delete(env, "ARKNET_TRACKER_SIGNING_KEY_FILE")
		}},
		{"insecure remote origin", func(env map[string]string) { env["ARKNET_CORE_WEB_SEED_ORIGIN"] = "http://core.example.test" }},
		{"wrong dev sidecar", func(env map[string]string) { env["ARKNET_CORE_WEB_SEED_ORIGIN"] = "http://arknet-core-extra:8080" }},
		{"origin path", func(env map[string]string) { env["ARKNET_CORE_WEB_SEED_ORIGIN"] = "https://core.example.test/base" }},
		{"relative root", func(env map[string]string) { env["ARKNET_CORE_WEB_SEED_ROOT"] = "media" }},
		{"unclean root", func(env map[string]string) {
			env["ARKNET_CORE_WEB_SEED_ROOT"] = root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root)
		}},
		{"filesystem root", func(env map[string]string) { env["ARKNET_CORE_WEB_SEED_ROOT"] = string(filepath.Separator) }},
		{"file root", func(env map[string]string) { env["ARKNET_CORE_WEB_SEED_ROOT"] = keyPath }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := make(map[string]string, len(base))
			for key, value := range base {
				env[key] = value
			}
			test.change(env)
			if _, err := loadConfig(func(key string) string { return env[key] }); err == nil {
				t.Fatal("expected web seed configuration error")
			}
		})
	}
}

func TestLoadConfigAllowsHTTPWebSeedOnlyForLoopbackAndExactCoreSidecar(t *testing.T) {
	keyPath := writeTrackerSigningKey(t)
	for _, origin := range []string{"http://127.0.0.1:8080", "http://[::1]:8080", "http://localhost:8080", "http://arknet-core:8080", "http://ARKNET-CORE:8080"} {
		t.Run(origin, func(t *testing.T) {
			env := map[string]string{
				"ARKNET_TRACKER_ANNOUNCE_URL":     "https://tracker.example.test",
				"ARKNET_TRACKER_SIGNING_KEY_FILE": keyPath,
				"ARKNET_CORE_WEB_SEED_ORIGIN":     origin,
				"ARKNET_CORE_WEB_SEED_ROOT":       t.TempDir(),
			}
			if _, err := loadConfig(func(key string) string { return env[key] }); err != nil {
				t.Fatal(err)
			}
		})
	}
}
