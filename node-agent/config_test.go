package main

import (
	"log/slog"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != "127.0.0.1:8081" || cfg.logLevel != slog.LevelInfo || cfg.shutdownTimeout != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	env := map[string]string{
		"ARKNET_AGENT_ADDR":             ":9000",
		"ARKNET_AGENT_LOG_LEVEL":        "debug",
		"ARKNET_AGENT_SHUTDOWN_TIMEOUT": "2s",
	}
	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.addr != ":9000" || cfg.logLevel != slog.LevelDebug || cfg.shutdownTimeout != 2*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadConfigRejectsInvalidValues(t *testing.T) {
	for _, tt := range []struct{ key, value string }{
		{"ARKNET_AGENT_ADDR", "missing-port"},
		{"ARKNET_AGENT_ADDR", "localhost:bad"},
		{"ARKNET_AGENT_ADDR", ":65536"},
		{"ARKNET_AGENT_LOG_LEVEL", "verbose"},
		{"ARKNET_AGENT_SHUTDOWN_TIMEOUT", "not-a-duration"},
		{"ARKNET_AGENT_SHUTDOWN_TIMEOUT", "0s"},
		{"ARKNET_AGENT_SHUTDOWN_TIMEOUT", "-1s"},
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
	env := map[string]string{"PGHOST": "postgres", "PGPORT": "5432", "PGDATABASE": "arknet_node", "PGUSER": "arknet", "PGPASSWORD": "special:@/?# password", "PGSSLMODE": "disable"}
	cfg, err := loadConfig(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(cfg.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if u.Host != "postgres:5432" || u.Path != "/arknet_node" || u.User.Username() != "arknet" || password != env["PGPASSWORD"] || u.Query().Get("sslmode") != "disable" {
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
