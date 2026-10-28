package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeKey(t *testing.T) string {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "verify-key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPath
}

func TestLoadAppliesValidatedOverrides(t *testing.T) {
	t.Setenv("ARKNET_TRACKER_VERIFY_KEY_FILE", writeKey(t))
	t.Setenv("ARKNET_TRACKER_BIND_ADDR", "[::1]:9000")
	t.Setenv("ARKNET_TRACKER_ANNOUNCE_INTERVAL", "5m")
	t.Setenv("ARKNET_TRACKER_PEER_TTL", "7m")
	t.Setenv("ARKNET_TRACKER_SHUTDOWN_TIMEOUT", "3s")
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.BindAddr != "[::1]:9000" || got.AnnounceInterval != 5*time.Minute || got.PeerTTL != 7*time.Minute || got.ShutdownTimeout != 3*time.Second {
		t.Fatalf("unexpected config: %+v", got)
	}
}

func TestLoadRejectsInvalidConfiguration(t *testing.T) {
	tests := map[string]struct {
		name  string
		value string
	}{
		"missing key file":  {name: "ARKNET_TRACKER_VERIFY_KEY_FILE", value: ""},
		"bad bind":          {name: "ARKNET_TRACKER_BIND_ADDR", value: "localhost"},
		"bad interval":      {name: "ARKNET_TRACKER_ANNOUNCE_INTERVAL", value: "soon"},
		"zero peer ttl":     {name: "ARKNET_TRACKER_PEER_TTL", value: "0s"},
		"negative shutdown": {name: "ARKNET_TRACKER_SHUTDOWN_TIMEOUT", value: "-1s"},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("ARKNET_TRACKER_VERIFY_KEY_FILE", writeKey(t))
			t.Setenv("ARKNET_TRACKER_BIND_ADDR", "127.0.0.1:8080")
			t.Setenv("ARKNET_TRACKER_ANNOUNCE_INTERVAL", "1m")
			t.Setenv("ARKNET_TRACKER_PEER_TTL", "2m")
			t.Setenv("ARKNET_TRACKER_SHUTDOWN_TIMEOUT", "1s")
			t.Setenv(testCase.name, testCase.value)
			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil")
			}
		})
	}
}

func TestLoadRejectsNonCanonicalVerificationKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "verify-key")
	if err := os.WriteFile(keyPath, []byte("not-base64"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARKNET_TRACKER_VERIFY_KEY_FILE", keyPath)
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil")
	}
}

func TestLoadUsesSafeDefaultsAndBase64VerificationKey(t *testing.T) {
	key := make([]byte, ed25519.PublicKeySize)
	for i := range key {
		key[i] = byte(i)
	}
	keyPath := filepath.Join(t.TempDir(), "verify-key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARKNET_TRACKER_VERIFY_KEY_FILE", keyPath)
	t.Setenv("ARKNET_TRACKER_BIND_ADDR", "")
	t.Setenv("ARKNET_TRACKER_ANNOUNCE_INTERVAL", "")
	t.Setenv("ARKNET_TRACKER_PEER_TTL", "")
	t.Setenv("ARKNET_TRACKER_SHUTDOWN_TIMEOUT", "")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.BindAddr != "127.0.0.1:8080" {
		t.Fatalf("BindAddr = %q", got.BindAddr)
	}
	if got.AnnounceInterval != 30*time.Minute || got.PeerTTL != 45*time.Minute || got.ShutdownTimeout != 10*time.Second {
		t.Fatalf("durations = %v, %v, %v", got.AnnounceInterval, got.PeerTTL, got.ShutdownTimeout)
	}
	if string(got.VerifyKey) != string(key) {
		t.Fatalf("VerifyKey does not match")
	}
}
