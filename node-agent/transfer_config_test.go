package main

import (
	"strings"
	"testing"
)

func TestTransferServiceConfig(t *testing.T) {
	for _, tt := range []struct {
		name, url, token string
		valid            bool
	}{
		{"disabled", "", "", true},
		{"sidecar", "http://arknet-transfer:8084", strings.Repeat("a", 32), true},
		{"loopback", "http://127.0.0.1:8084", strings.Repeat("b", 32), true},
		{"missing token", "http://127.0.0.1:8084", "", false},
		{"missing URL", "", strings.Repeat("c", 32), false},
		{"short token", "http://127.0.0.1:8084", "short", false},
		{"query", "http://127.0.0.1:8084?secret=x", strings.Repeat("a", 32), false},
		{"userinfo", "http://private:password@127.0.0.1:8084", strings.Repeat("a", 32), false},
		{"token newline", "http://127.0.0.1:8084", strings.Repeat("a", 32) + "\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				switch key {
				case "ARKNET_TRANSFER_URL":
					return tt.url
				case "ARKNET_TRANSFER_TOKEN":
					return tt.token
				}
				return ""
			})
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v error=%v", tt.valid, err)
			}
			if tt.valid && (cfg.transferURL != tt.url || cfg.transferToken != tt.token) {
				t.Fatal("transfer configuration lost")
			}
			if err != nil && (strings.Contains(err.Error(), tt.token) && tt.token != "" || strings.Contains(err.Error(), "password")) {
				t.Fatal("configuration error exposed private input")
			}
		})
	}
}

func TestTransferFinalizerConfig(t *testing.T) {
	cfg, err := loadConfig(func(key string) string {
		values := map[string]string{
			"ARKNET_TRANSFER_URL":                "http://arknet-transfer:8084",
			"ARKNET_TRANSFER_TOKEN":              strings.Repeat("a", 32),
			"ARKNET_TRANSFER_STAGING_DIR":        "/srv/arknet/transfers",
			"ARKNET_MEDIA_DIR":                   "/srv/arknet/media",
			"JELLYFIN_API_URL":                   "http://jellyfin:8096",
			"JELLYFIN_API_KEY":                   strings.Repeat("j", 32),
			"ARKNET_TRANSFER_RECONCILE_INTERVAL": "45s",
		}
		return values[key]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.transferStagingDir != "/srv/arknet/transfers" || cfg.mediaDir != "/srv/arknet/media" || cfg.jellyfinAPIKey != strings.Repeat("j", 32) || cfg.transferReconcileInterval.String() != "45s" {
		t.Fatalf("finalizer config lost: %+v", cfg)
	}
	for _, key := range []string{"ARKNET_TRANSFER_STAGING_DIR", "ARKNET_MEDIA_DIR"} {
		_, err := loadConfig(func(candidate string) string {
			if candidate == key {
				return "relative/path"
			}
			return ""
		})
		if err == nil {
			t.Fatalf("accepted relative %s", key)
		}
	}
}
