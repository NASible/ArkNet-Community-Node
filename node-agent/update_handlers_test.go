package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"arknet/node-agent/internal/release"
	"arknet/node-agent/internal/updates"
)

type memoryUpdateSettings struct {
	enabled *bool
	fail    bool
}

func (m *memoryUpdateSettings) AutomaticUpdates(context.Context) (bool, error) {
	if m.fail {
		return false, errors.New("db down")
	}
	if m.enabled == nil {
		return true, nil
	}
	return *m.enabled, nil
}
func (m *memoryUpdateSettings) SetAutomaticUpdates(_ context.Context, enabled bool) error {
	if m.fail {
		return errors.New("db down")
	}
	m.enabled = &enabled
	return nil
}

// newerRelease publishes a valid newer manifest derived from the shipped one.
func newerRelease(t *testing.T, newVersion string) (*httptest.Server, *int) {
	t.Helper()
	shipped, err := os.ReadFile(shippedManifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest := []byte(strings.ReplaceAll(string(shipped), version, newVersion))
	sum := sha256.Sum256(manifest)
	hits := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		switch r.URL.Path {
		case "/index.json":
			_, _ = w.Write([]byte(`{"schema":"arknet.node-release-index/v1","channels":{"development":{"version":"` + newVersion +
				`","manifest_url":"release-` + newVersion + `.json","manifest_sha256":"` + hex.EncodeToString(sum[:]) + `"}}}`))
		case "/release-" + newVersion + ".json":
			_, _ = w.Write(manifest)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, hits
}

func updatesHandler(t *testing.T, indexURL string, settings *memoryUpdateSettings) http.Handler {
	t.Helper()
	current, err := release.LoadCurrent(shippedManifest, version)
	if err != nil {
		t.Fatal(err)
	}
	var checker *updates.Checker
	if indexURL != "" {
		u, _ := url.Parse(indexURL)
		checker = &updates.Checker{IndexURL: u}
	}
	return newRoutes(testRepository{}, releaseOpt{&current}, updatesOpt{monitor: updates.NewMonitor(checker, &current), settings: settings})
}

func updatesCall(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest(method, path, body))
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w.Code, decoded
}

func TestUpdateDiscoveryReportsNewerTestRelease(t *testing.T) {
	server, _ := newerRelease(t, "0.0.2-dev")
	h := updatesHandler(t, server.URL+"/index.json", &memoryUpdateSettings{})
	code, body := updatesCall(t, h, "GET", "/api/v1/updates", "")
	if code != 200 || body["current_version"] != version || body["channel"] != "development" || body["available"] != nil ||
		body["automatic_updates"] != true || body["automatic_install_supported"] != false || body["discovery"].(map[string]any)["state"] != "NOT_CHECKED" {
		t.Fatalf("before check: %d %v", code, body)
	}
	code, body = updatesCall(t, h, "POST", "/api/v1/updates/check", "{}")
	available, _ := body["available"].(map[string]any)
	discovery := body["discovery"].(map[string]any)
	if code != 200 || discovery["state"] != "UPDATE_AVAILABLE" || discovery["checked_at"] == nil || available["version"] != "0.0.2-dev" || available["channel"] != "development" {
		t.Fatalf("after check: %d %v", code, body)
	}
	if _, body = updatesCall(t, h, "GET", "/api/v1/updates", ""); body["available"].(map[string]any)["version"] != "0.0.2-dev" {
		t.Fatalf("cached: %v", body)
	}
}

func TestUpdateCheckFailureIsReportedNotHidden(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	h := updatesHandler(t, server.URL+"/index.json", &memoryUpdateSettings{})
	code, body := updatesCall(t, h, "POST", "/api/v1/updates/check", "{}")
	if code != 200 || body["discovery"].(map[string]any)["state"] != "CHECK_FAILED" || body["available"] != nil {
		t.Fatalf("failure: %d %v", code, body)
	}
	_, body = updatesCall(t, updatesHandler(t, "", &memoryUpdateSettings{}), "POST", "/api/v1/updates/check", "{}")
	if body["discovery"].(map[string]any)["state"] != "NOT_CONFIGURED" {
		t.Fatalf("unconfigured: %v", body)
	}
}

func TestAutomaticUpdatesDefaultOnAndOperatorCanDisable(t *testing.T) {
	settings := &memoryUpdateSettings{}
	h := updatesHandler(t, "", settings)
	if code, body := updatesCall(t, h, "GET", "/api/v1/settings/automatic-updates", ""); code != 200 || body["enabled"] != true {
		t.Fatalf("default: %d %v", code, body)
	}
	if code, body := updatesCall(t, h, "PUT", "/api/v1/settings/automatic-updates", `{"enabled":false}`); code != 200 || body["enabled"] != false || settings.enabled == nil || *settings.enabled {
		t.Fatalf("disable: %d %v", code, body)
	}
	if _, body := updatesCall(t, h, "GET", "/api/v1/updates", ""); body["automatic_updates"] != false {
		t.Fatalf("updates view after disable: %v", body)
	}
	for _, bad := range []string{`{}`, `{"enabled":"false"}`, `{"enabled":0}`, `{"enabled":false,"x":1}`, `[]`, `{"enabled":false}{}`} {
		if code, _ := updatesCall(t, h, "PUT", "/api/v1/settings/automatic-updates", bad); code != 400 {
			t.Fatalf("%s: %d", bad, code)
		}
	}
	for _, path := range []string{"/api/v1/updates?channel=stable", "/api/v1/updates/check?force=1"} {
		method := map[bool]string{true: "GET", false: "POST"}[strings.HasSuffix(path, "stable")]
		if code, _ := updatesCall(t, h, method, path, "{}"); code != 400 {
			t.Fatalf("%s %s: %d", method, path, code)
		}
	}
	for _, bad := range []string{``, `{"force":true}`, `null`} {
		if code, _ := updatesCall(t, h, "POST", "/api/v1/updates/check", bad); code != 400 {
			t.Fatalf("check body %q: %d", bad, code)
		}
	}
	r := selectionRequest("PUT", "/api/v1/settings/automatic-updates", `{"enabled":true}`)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || *settings.enabled {
		t.Fatalf("cross-origin change: %d", w.Code)
	}
	settings.fail = true
	if code, _ := updatesCall(t, h, "GET", "/api/v1/updates", ""); code != 503 {
		t.Fatalf("db down: %d", code)
	}
}

func TestUpdatesWithoutCurrentReleaseNeverFetch(t *testing.T) {
	server, hits := newerRelease(t, "0.0.2-dev")
	u, _ := url.Parse(server.URL + "/index.json")
	h := newRoutes(testRepository{}, updatesOpt{monitor: updates.NewMonitor(&updates.Checker{IndexURL: u}, nil), settings: &memoryUpdateSettings{}})
	code, body := updatesCall(t, h, "POST", "/api/v1/updates/check", "{}")
	if code != 200 || body["current_version"] != nil || body["channel"] != nil || body["discovery"].(map[string]any)["state"] != "NO_CURRENT_RELEASE" || *hits != 0 {
		t.Fatalf("no release: %d %v hits=%d", code, body, *hits)
	}
}

func TestReleaseIndexConfig(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://u:p@localhost/db"}
	for _, test := range []struct {
		url, allow string
		ok         bool
	}{
		{"", "", true}, {"https://releases.arknet.example/node/index.json", "", true}, {"http://127.0.0.1:9/index.json", "", true},
		{"http://release-index:8080/index.json", "", false}, {"http://release-index:8080/index.json", "true", true},
		{"https://u:p@releases.example/i.json", "", false}, {"releases.example/i.json", "", false},
	} {
		env := map[string]string{"ARKNET_RELEASE_INDEX_URL": test.url, "ARKNET_RELEASE_INDEX_ALLOW_HTTP": test.allow}
		for k, v := range base {
			env[k] = v
		}
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if (err == nil) != test.ok || (test.ok && test.url != "" && cfg.releaseIndexURL.String() != test.url) {
			t.Fatalf("%+v: %v", test, err)
		}
	}
}
