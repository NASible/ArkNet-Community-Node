package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguration(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{
		"ARKNET_TRANSFER_TOKEN":           testToken,
		"ARKNET_TRANSFER_TRACKER_ORIGIN":  "https://tracker.arknet.test",
		"ARKNET_TRANSFER_STAGING_DIR":     filepath.Join(root, "staging"),
		"ARKNET_TRANSFER_STATE_DIR":       filepath.Join(root, "state"),
		"ARKNET_TRANSFER_WORKER":          filepath.Join(root, "worker.py"),
		"ARKNET_TRANSFER_WEB_SEED_ORIGIN": "https://core.arknet.test:8443",
	}
	get := func(k string) string { return env[k] }
	cfg, err := loadConfig(get)
	if err != nil || cfg.addr != "127.0.0.1:8084" || cfg.token != testToken || cfg.trackerOrigin != env["ARKNET_TRANSFER_TRACKER_ORIGIN"] || cfg.webSeedOrigin != env["ARKNET_TRANSFER_WEB_SEED_ORIGIN"] || cfg.webSeedAllowHTTP || cfg.stagingDir != env["ARKNET_TRANSFER_STAGING_DIR"] || cfg.stateDir != env["ARKNET_TRANSFER_STATE_DIR"] || cfg.workerPath != env["ARKNET_TRANSFER_WORKER"] {
		t.Fatalf("config: %+v err=%v", cfg, err)
	}
	if cfg.downloadRateLimit != 0 || cfg.listenInterfaces != "0.0.0.0:0" {
		t.Fatalf("defaults: %+v", cfg)
	}

	env["ARKNET_TRANSFER_ADDR"] = "127.0.0.1:0"
	env["ARKNET_TRANSFER_TRACKER_ORIGIN"] = "http://tracker:8080"
	env["ARKNET_TRANSFER_TRACKER_ALLOW_HTTP"] = "true"
	env["ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT"] = "262144"
	env["ARKNET_TRANSFER_LISTEN_INTERFACES"] = "0.0.0.0:6881"
	env["ARKNET_TRANSFER_WEB_SEED_ORIGIN"] = "http://127.0.0.1:8080"
	env["ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP"] = "true"
	cfg, err = loadConfig(get)
	if err != nil || cfg.addr != "127.0.0.1:0" || cfg.downloadRateLimit != 262144 || !cfg.trackerAllowHTTP || !cfg.webSeedAllowHTTP || cfg.listenInterfaces != "0.0.0.0:6881" {
		t.Fatalf("overrides: %+v err=%v", cfg, err)
	}
}

func TestConfigurationRejectsUnsafeWebSeedOriginPolicy(t *testing.T) {
	root := t.TempDir()
	base := map[string]string{
		"ARKNET_TRANSFER_TOKEN":          testToken,
		"ARKNET_TRANSFER_TRACKER_ORIGIN": "https://tracker.arknet.test",
		"ARKNET_TRANSFER_STAGING_DIR":    filepath.Join(root, "staging"),
		"ARKNET_TRANSFER_STATE_DIR":      filepath.Join(root, "state"),
		"ARKNET_TRANSFER_WORKER":         filepath.Join(root, "worker.py"),
	}
	check := func(origin, allow string, valid bool) {
		t.Helper()
		env := make(map[string]string, len(base)+2)
		for key, value := range base {
			env[key] = value
		}
		env["ARKNET_TRANSFER_WEB_SEED_ORIGIN"] = origin
		env["ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP"] = allow
		_, err := loadConfig(func(key string) string { return env[key] })
		if (err == nil) != valid || (err != nil && origin != "" && strings.Contains(err.Error(), origin)) {
			t.Fatalf("origin=%q allow=%q valid=%v err=%v", origin, allow, valid, err)
		}
	}
	check("", "", true)
	check("https://core.arknet.test", "", true)
	check("http://core.arknet.test:8080", "true", true)
	for _, pair := range [][2]string{
		{"", "true"}, {"https://core.arknet.test", "TRUE"}, {"http://core.arknet.test", ""},
		{"ftp://core.arknet.test", ""}, {"https://user:secret@core.arknet.test", ""},
		{"https://core.arknet.test/", ""}, {"https://core.arknet.test/path", ""},
		{"https://core.arknet.test?x=1", ""}, {"https://core.arknet.test#x", ""},
		{"https://core.arknet.test:", ""}, {"https://core.arknet.test:0", ""},
	} {
		check(pair[0], pair[1], false)
	}
}

func TestConfigurationRejectsUnsafeTransferSettings(t *testing.T) {
	root := t.TempDir()
	base := map[string]string{
		"ARKNET_TRANSFER_TOKEN":          testToken,
		"ARKNET_TRANSFER_TRACKER_ORIGIN": "https://tracker.arknet.test",
		"ARKNET_TRANSFER_STAGING_DIR":    filepath.Join(root, "staging"),
		"ARKNET_TRANSFER_STATE_DIR":      filepath.Join(root, "state"),
		"ARKNET_TRANSFER_WORKER":         filepath.Join(root, "worker.py"),
	}
	check := func(key, value string) {
		t.Helper()
		env := make(map[string]string, len(base)+1)
		for k, v := range base {
			env[k] = v
		}
		env[key] = value
		if _, err := loadConfig(func(k string) string { return env[k] }); err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("accepted %s=%q or leaked token", key, value)
		}
	}
	for _, value := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 257), testToken + " ", testToken + "\r\n", testToken + "\t", testToken + "\x7f", testToken + "é"} {
		check("ARKNET_TRANSFER_TOKEN", value)
	}
	for _, value := range []string{"", "ftp://tracker", "http://tracker", "https://user:secret@tracker", "https://tracker/announce", "https://tracker#fragment", "https://tracker?token=x"} {
		check("ARKNET_TRANSFER_TRACKER_ORIGIN", value)
	}
	for _, value := range []string{"1", "TRUE", "yes"} {
		check("ARKNET_TRANSFER_TRACKER_ALLOW_HTTP", value)
	}
	for _, key := range []string{"ARKNET_TRANSFER_STAGING_DIR", "ARKNET_TRANSFER_STATE_DIR", "ARKNET_TRANSFER_WORKER"} {
		for _, value := range []string{"", "relative/path", ".", "../escape"} {
			check(key, value)
		}
	}
	for _, value := range []string{"-1", "abc", "1.5", "9223372036854775808"} {
		check("ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT", value)
	}
	for _, value := range []string{"bad", ":6881", "localhost:6881", "0.0.0.0:-1", "0.0.0.0:65536"} {
		check("ARKNET_TRANSFER_LISTEN_INTERFACES", value)
	}
	for _, value := range []string{"bad", ":8084", "127.0.0.1:bad", "localhost:8084", "127.0.0.1:65536", "127.0.0.1:-1"} {
		check("ARKNET_TRANSFER_ADDR", value)
	}
}
