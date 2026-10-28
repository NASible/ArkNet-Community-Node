package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"arknet/node-agent/internal/release"
)

const shippedManifest = "../deploy/node/release.json"

// The shipped manifest is the single source of component versions: it must
// match VERSION, this Agent build, the Compose files byte-for-byte, and every
// image the Compose files run.
func TestShippedReleaseManifestDescribesThisStack(t *testing.T) {
	m, err := release.LoadCurrent(shippedManifest, version)
	if err != nil {
		t.Fatalf("Agent cannot parse its current release manifest: %v", err)
	}
	stackVersion, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != strings.TrimSpace(string(stackVersion)) {
		t.Fatalf("manifest %s differs from VERSION", m.Version)
	}
	var ui struct {
		Version string `json:"version"`
	}
	data, err := os.ReadFile("../node-ui/package.json")
	if err != nil || json.Unmarshal(data, &ui) != nil || ui.Version != m.Components[release.UI].Version {
		t.Fatalf("node-ui package version %q differs from manifest", ui.Version)
	}

	images := map[string]bool{}
	imageLine := regexp.MustCompile(`(?m)^\s+image:\s*(\S+)\s*$`)
	for _, f := range m.Compose.Files {
		data, err := os.ReadFile(filepath.Join("../deploy/node", f.Path))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			t.Fatalf("%s changed without updating release.json (sha256 %x)", f.Path, sum)
		}
		for _, match := range imageLine.FindAllSubmatch(data, -1) {
			ref := string(match[1])
			if strings.Contains(ref, "$") {
				t.Fatalf("%s lets operators override image %s", f.Path, ref)
			}
			images[ref] = true
		}
	}
	var bundled []string
	for name, c := range m.Components {
		if c.Bundled {
			bundled = append(bundled, c.Image)
			if !images[c.Image] {
				t.Errorf("bundled component %s image %s is not in the release Compose files", name, c.Image)
			}
		} else if images[c.Image] {
			t.Errorf("unbundled component %s appears in Compose", name)
		}
	}
	var composeImages []string
	for ref := range images {
		composeImages = append(composeImages, ref)
	}
	sort.Strings(bundled)
	sort.Strings(composeImages)
	if strings.Join(bundled, ",") != strings.Join(composeImages, ",") {
		t.Fatalf("Compose images %v differ from bundled manifest images %v", composeImages, bundled)
	}
}

func TestStatusReportsCurrentRelease(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	current := loadCurrentRelease(mustAbs(t, shippedManifest), logger)
	if current.manifest == nil || !strings.Contains(logs.String(), `"msg":"release_manifest_loaded"`) {
		t.Fatalf("shipped manifest not loaded: %s", logs.String())
	}
	w := httptest.NewRecorder()
	newHandler(testRepository{}, current).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	var status struct {
		Version string         `json:"version"`
		Release map[string]any `json:"release"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || w.Code != 200 {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if len(status.Release) != 2 || status.Release["version"] != version || status.Release["channel"] != "development" {
		t.Fatalf("release = %v", status.Release)
	}
}

func TestInvalidOrMismatchedManifestKeepsAgentRunningWithoutRelease(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile(shippedManifest)
	if err != nil {
		t.Fatal(err)
	}
	mismatched := bytes.ReplaceAll(data, []byte(version), []byte("9.9.9-dev"))
	cases := map[string][]byte{"invalid": []byte(`{"schema":"x"}`), "mismatch": mismatched}
	for name, content := range cases {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		var logs bytes.Buffer
		current := loadCurrentRelease(path, slog.New(slog.NewJSONHandler(&logs, nil)))
		want := map[string]string{"invalid": "release_manifest_invalid", "mismatch": "release_manifest_mismatch"}[name]
		if current.manifest != nil || !strings.Contains(logs.String(), want) {
			t.Fatalf("%s: manifest=%v logs=%s", name, current.manifest, logs.String())
		}
		w := httptest.NewRecorder()
		newHandler(testRepository{}, current).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"release":null`) {
			t.Fatalf("%s status: %d %s", name, w.Code, w.Body)
		}
	}
	var logs bytes.Buffer
	if current := loadCurrentRelease("", slog.New(slog.NewJSONHandler(&logs, nil))); current.manifest != nil || !strings.Contains(logs.String(), "release_manifest_not_configured") {
		t.Fatal("unconfigured manifest")
	}
}

func TestReleaseManifestConfig(t *testing.T) {
	base := map[string]string{"DATABASE_URL": "postgres://u:p@localhost/db"}
	for value, ok := range map[string]bool{"": true, "/etc/arknet/release.json": true, "release.json": false, "/etc/arknet/../release.json": false} {
		env := map[string]string{"ARKNET_RELEASE_MANIFEST": value}
		for k, v := range base {
			env[k] = v
		}
		cfg, err := loadConfig(func(k string) string { return env[k] })
		if (err == nil) != ok || (ok && cfg.releaseManifest != value) {
			t.Fatalf("%q: cfg=%q err=%v", value, cfg.releaseManifest, err)
		}
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
