package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayKeysLoadWithoutGivingTrackerAnyPrivateKey(t *testing.T) {
	t.Setenv("ARKNET_TRACKER_VERIFY_KEY_FILE", writeKey(t))
	first, _, _ := ed25519.GenerateKey(nil)
	second, _, _ := ed25519.GenerateKey(nil)
	path := filepath.Join(t.TempDir(), "relay-public-keys")
	text := base64.StdEncoding.EncodeToString(first) + "\n" + base64.StdEncoding.EncodeToString(second) + "\n"
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE", path)
	cfg, err := Load()
	if err != nil || len(cfg.RelayVerifyKeys) != 2 || !cfg.RelayVerifyKeys[1].Equal(second) {
		t.Fatalf("keys %v %v", cfg.RelayVerifyKeys, err)
	}
}
func TestRelayKeysAreOptionalButConfiguredFileMustBeValid(t *testing.T) {
	t.Setenv("ARKNET_TRACKER_VERIFY_KEY_FILE", writeKey(t))
	t.Setenv("ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE", "")
	cfg, err := Load()
	if err != nil || len(cfg.RelayVerifyKeys) != 0 {
		t.Fatalf("%v %v", cfg, err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	key := base64.StdEncoding.EncodeToString(pub)
	for name, text := range map[string]string{
		"empty": "", "garbage": "not a key", "duplicate": key + "\n" + key, "private key": base64.StdEncoding.EncodeToString(priv),
		"grant key": base64.StdEncoding.EncodeToString(make([]byte, 32)), "too many": strings.Repeat(key+"\n", 17), "too large": strings.Repeat("a", 16385),
		"noncanonical": strings.TrimRight(key, "="),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "keys")
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE", path)
			if _, err := Load(); err == nil {
				t.Fatal("invalid relay keys accepted")
			}
		})
	}
	t.Setenv("ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := Load(); err == nil {
		t.Fatal("missing file accepted")
	}
}
