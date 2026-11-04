package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writePrivateKey(t *testing.T, dir, name string) (string, ed25519.PrivateKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(private)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, private
}

type relayFixture struct {
	dir     string
	env     map[string]string
	entries []map[string]string
	tracker string
}

func newRelayFixture(t *testing.T) *relayFixture {
	t.Helper()
	dir := t.TempDir()
	trackerKey, _ := writePrivateKey(t, dir, "tracker.key")
	listKey, _ := writePrivateKey(t, dir, "list.key")
	f := &relayFixture{dir: dir, tracker: trackerKey, env: map[string]string{
		"ARKNET_TRACKER_ANNOUNCE_URL":        "https://tracker.example",
		"ARKNET_TRACKER_SIGNING_KEY_FILE":    trackerKey,
		"ARKNET_RELAY_LIST_SIGNING_KEY_FILE": listKey,
		"ARKNET_RELAYS_FILE":                 filepath.Join(dir, "relays.json"),
	}}
	for i, id := range []string{"relay-dc-1", "relay-west-1"} {
		key, _ := writePrivateKey(t, dir, id+".key")
		f.entries = append(f.entries, map[string]string{"id": id, "address": []string{"relay1.example.net:7443", "203.0.113.7:7443"}[i],
			"spki_sha256": strings.Repeat([]string{"a", "b"}[i], 64), "control_url": "http://10.0.0." + string(rune('5'+i)) + ":7080",
			"revocation_key_file": key})
	}
	return f
}

func (f *relayFixture) load(t *testing.T) (config, error) {
	t.Helper()
	data, _ := json.Marshal(f.entries)
	if err := os.WriteFile(filepath.Join(f.dir, "relays.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PGPASSWORD": "x"}
	for k, v := range f.env {
		env[k] = v
	}
	return loadConfig(func(name string) string { return env[name] })
}

func TestRelaysAreOptIn(t *testing.T) {
	cfg, err := loadConfig(func(name string) string { return map[string]string{"PGPASSWORD": "x"}[name] })
	if err != nil || cfg.relays != nil {
		t.Fatalf("%v %v", cfg.relays, err)
	}
}

func TestRelayConfigurationLoads(t *testing.T) {
	cfg, err := newRelayFixture(t).load(t)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.relays
	if r == nil || len(r.entries) != 2 || r.entries[0].ID != "relay-dc-1" || r.entries[1].Address != "203.0.113.7:7443" ||
		r.entries[0].SPKISHA256 != strings.Repeat("a", 64) || r.entries[1].ControlURL.String() != "http://10.0.0.6:7080" ||
		len(r.entries[0].revocationKey) != ed25519.PrivateKeySize || len(r.listKey) != ed25519.PrivateKeySize {
		t.Fatalf("%+v", r)
	}
}

func TestRelayKeysMustBeDistinct(t *testing.T) {
	for name, change := range map[string]func(*relayFixture){
		"revocation key is the tracker key": func(f *relayFixture) { f.entries[0]["revocation_key_file"] = f.tracker },
		"list key is the tracker key":       func(f *relayFixture) { f.env["ARKNET_RELAY_LIST_SIGNING_KEY_FILE"] = f.tracker },
		"two relays share a revocation key": func(f *relayFixture) { f.entries[1]["revocation_key_file"] = f.entries[0]["revocation_key_file"] },
		"revocation key is the list key": func(f *relayFixture) {
			f.entries[0]["revocation_key_file"] = f.env["ARKNET_RELAY_LIST_SIGNING_KEY_FILE"]
		},
		"revocation key is the assertion key": func(f *relayFixture) {
			f.env["ARKNET_ASSERTION_SIGNING_KEY_FILE"] = f.entries[1]["revocation_key_file"]
			f.env["ARKNET_ASSERTION_PUBLISHED_KEYS_FILE"] = filepath.Join(f.dir, "published")
			_ = os.WriteFile(f.env["ARKNET_ASSERTION_PUBLISHED_KEYS_FILE"], nil, 0o600)
		},
		"list key is the assertion key": func(f *relayFixture) {
			f.env["ARKNET_ASSERTION_SIGNING_KEY_FILE"] = f.env["ARKNET_RELAY_LIST_SIGNING_KEY_FILE"]
			f.env["ARKNET_ASSERTION_PUBLISHED_KEYS_FILE"] = filepath.Join(f.dir, "published")
			_ = os.WriteFile(f.env["ARKNET_ASSERTION_PUBLISHED_KEYS_FILE"], nil, 0o600)
		},
	} {
		f := newRelayFixture(t)
		change(f)
		if _, err := f.load(t); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRelayConfigurationRejectsUnsafeEntries(t *testing.T) {
	for name, change := range map[string]func(*relayFixture){
		"no list key":    func(f *relayFixture) { delete(f.env, "ARKNET_RELAY_LIST_SIGNING_KEY_FILE") },
		"no relays file": func(f *relayFixture) { delete(f.env, "ARKNET_RELAYS_FILE") },
		"no tracker": func(f *relayFixture) {
			delete(f.env, "ARKNET_TRACKER_ANNOUNCE_URL")
			delete(f.env, "ARKNET_TRACKER_SIGNING_KEY_FILE")
		},
		"empty list":           func(f *relayFixture) { f.entries = nil },
		"duplicate ID":         func(f *relayFixture) { f.entries[1]["id"] = "relay-dc-1" },
		"bad ID":               func(f *relayFixture) { f.entries[0]["id"] = "Relay One" },
		"address without port": func(f *relayFixture) { f.entries[0]["address"] = "relay1.example.net" },
		"address with zero":    func(f *relayFixture) { f.entries[0]["address"] = "relay1.example.net:0" },
		"address with path":    func(f *relayFixture) { f.entries[0]["address"] = "relay1.example.net:7443/x" },
		"short pin":            func(f *relayFixture) { f.entries[0]["spki_sha256"] = "abcd" },
		"uppercase pin":        func(f *relayFixture) { f.entries[0]["spki_sha256"] = strings.Repeat("A", 64) },
		"control URL path":     func(f *relayFixture) { f.entries[0]["control_url"] = "http://10.0.0.5:7080/admin" },
		"control URL userinfo": func(f *relayFixture) { f.entries[0]["control_url"] = "http://u:p@10.0.0.5:7080" },
		"control URL scheme":   func(f *relayFixture) { f.entries[0]["control_url"] = "ftp://10.0.0.5" },
		"missing key file":     func(f *relayFixture) { f.entries[0]["revocation_key_file"] = filepath.Join(f.dir, "nope") },
		"unknown field":        func(f *relayFixture) { f.entries[0]["weight"] = "3" },
		"too many relays": func(f *relayFixture) {
			for len(f.entries) <= maxConfiguredRelays {
				f.entries = append(f.entries, f.entries[0])
			}
		},
	} {
		f := newRelayFixture(t)
		change(f)
		if _, err := f.load(t); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
