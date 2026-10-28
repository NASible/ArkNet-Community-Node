package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWebSeedServesAuthorizedActiveObject(t *testing.T) {
	const content = "authorized web seed content"
	repository, _ := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	repository.objects[0].Size = int64(len(content))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, activeMediaID), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	issuer := testTrackerGrantIssuer(t)
	issuer.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	grant, _, err := issuer.issueGrant(enrollmentID, repository.objects[0].InfoHashV2, trackerGrantActionDownload)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://core.example.test")
	handler := newHandlerWithWebSeed(nil, issuer, &webSeedConfig{origin: origin, root: root}, repository)
	request := httptest.NewRequest(http.MethodGet, "/api/web-seed/v1/media/"+activeMediaID+"/"+grant, nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != content {
		t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" || response.Header().Get("Content-Type") != "application/octet-stream" || response.Header().Get("Last-Modified") != "" {
		t.Fatalf("headers = %v", response.Header())
	}
}

func TestTorrentMetadataServesAuthorizedObjectAndGrantAdvertisesURL(t *testing.T) {
	const metadata = "private v2 torrent metadata"
	repository, privateKey := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	issuer := testTrackerGrantIssuer(t)
	origin, _ := url.Parse("https://core.example.test:8443")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, activeMediaID+".torrent"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	request := signedRequest(http.MethodPost, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()
	handler := newHandlerWithWebSeed(nil, issuer, &webSeedConfig{origin: origin, root: root}, repository)

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		TrackerURL string `json:"tracker_url"`
		TorrentURL string `json:"torrent_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	tracker, _ := url.Parse(body.TrackerURL)
	grant := strings.TrimPrefix(tracker.Path, "/announce/")
	want := "https://core.example.test:8443/api/torrent/v1/media/" + activeMediaID + "/" + grant
	if body.TorrentURL != want || strings.Contains(body.TorrentURL, root) {
		t.Fatalf("torrent_url = %q, want %q", body.TorrentURL, want)
	}
	metadataResponse := httptest.NewRecorder()
	handler.ServeHTTP(metadataResponse, httptest.NewRequest(http.MethodGet, "/api/torrent/v1/media/"+activeMediaID+"/"+grant, nil))
	if metadataResponse.Code != http.StatusOK || metadataResponse.Body.String() != metadata {
		t.Fatalf("metadata status = %d, body = %q", metadataResponse.Code, metadataResponse.Body.String())
	}
	if metadataResponse.Header().Get("Content-Type") != "application/x-bittorrent" || metadataResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("metadata headers = %v", metadataResponse.Header())
	}
}

func TestTrackerGrantResponseAddsWebSeedURLWithSameToken(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	issuer := testTrackerGrantIssuer(t)
	origin, _ := url.Parse("https://core.example.test:8443")
	root := t.TempDir()
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	request := signedRequest(http.MethodPost, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()

	newHandlerWithWebSeed(nil, issuer, &webSeedConfig{origin: origin, root: root}, repository).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var body struct {
		TrackerURL string `json:"tracker_url"`
		WebSeedURL string `json:"web_seed_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	tracker, _ := url.Parse(body.TrackerURL)
	wantGrant := strings.TrimPrefix(tracker.Path, "/announce/")
	want := "https://core.example.test:8443/api/web-seed/v1/media/" + activeMediaID + "/" + wantGrant
	if body.WebSeedURL != want || strings.Contains(body.WebSeedURL, root) {
		t.Fatalf("web_seed_url = %q, want %q", body.WebSeedURL, want)
	}
}

func TestTrackerGrantSeedOnlyResponseOmitsWebSeedURL(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	issuer := testTrackerGrantIssuer(t)
	origin, _ := url.Parse("https://core.example.test")
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	body := `{"actions":["seed"]}`
	request := signedRequest(http.MethodPost, path, body, enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()

	newHandlerWithWebSeed(nil, issuer, &webSeedConfig{origin: origin, root: t.TempDir()}, &seedingMediaRepository{mediaRepository: repository, eligible: true}).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if _, present := result["web_seed_url"]; present {
		t.Fatalf("seed-only response advertised web seed: %s", response.Body.String())
	}
}

func newWebSeedFixture(t *testing.T, content string) (*mediaRepository, *trackerGrantIssuer, *webSeedConfig, string) {
	t.Helper()
	repository, _ := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	repository.objects[0].Size = int64(len(content))
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, activeMediaID), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	issuer := testTrackerGrantIssuer(t)
	issuer.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	grant, _, err := issuer.issueGrant(enrollmentID, repository.objects[0].InfoHashV2, trackerGrantActionDownload)
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://core.example.test")
	return repository, issuer, &webSeedConfig{origin: origin, root: root}, grant
}

func TestWebSeedSupportsHeadAndRFCByteRanges(t *testing.T) {
	const content = "0123456789abcdefghijklmnopqrstuvwxyz"
	tests := []struct {
		name, method, rangeHeader, wantRange, wantBody, wantLength string
		wantStatus                                                 int
	}{
		{"head", http.MethodHead, "", "", "", "36", http.StatusOK},
		{"closed", http.MethodGet, "bytes=3-7", "bytes 3-7/36", "34567", "5", http.StatusPartialContent},
		{"suffix", http.MethodGet, "bytes=-4", "bytes 32-35/36", "wxyz", "4", http.StatusPartialContent},
		{"open ended", http.MethodGet, "bytes=30-", "bytes 30-35/36", "uvwxyz", "6", http.StatusPartialContent},
		{"malformed", http.MethodGet, "bytes=bad", "", "invalid range\n", "", http.StatusRequestedRangeNotSatisfiable},
		{"unsatisfiable", http.MethodGet, "bytes=99-100", "bytes */36", "invalid range: failed to overlap\n", "", http.StatusRequestedRangeNotSatisfiable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, issuer, cfg, grant := newWebSeedFixture(t, content)
			request := httptest.NewRequest(test.method, "/api/web-seed/v1/media/"+activeMediaID+"/"+grant, nil)
			request.Header.Set("Range", test.rangeHeader)
			response := httptest.NewRecorder()
			newHandlerWithWebSeed(nil, issuer, cfg, repository).ServeHTTP(response, request)
			if response.Code != test.wantStatus || response.Body.String() != test.wantBody || response.Header().Get("Content-Range") != test.wantRange || response.Header().Get("Content-Length") != test.wantLength {
				t.Fatalf("status=%d range=%q length=%q body=%q", response.Code, response.Header().Get("Content-Range"), response.Header().Get("Content-Length"), response.Body.String())
			}
		})
	}
}

func TestWebSeedRejectsInvalidAuthorizationBeforeOpeningFile(t *testing.T) {
	tests := []struct {
		name            string
		grant           func(*testing.T, *trackerGrantIssuer, string) string
		wantCatalogRead bool
	}{
		{"malformed", func(_ *testing.T, _ *trackerGrantIssuer, _ string) string { return "not-a-grant" }, false},
		{"expired", func(t *testing.T, issuer *trackerGrantIssuer, hash string) string {
			grant, _, err := issuer.issueGrant(enrollmentID, hash, trackerGrantActionDownload)
			if err != nil {
				t.Fatal(err)
			}
			issuer.now = func() time.Time { return time.Unix(2_000_000_301, 0) }
			return grant
		}, false},
		{"seed only", func(t *testing.T, issuer *trackerGrantIssuer, hash string) string {
			grant, _, err := issuer.issueGrant(enrollmentID, hash, trackerGrantActionSeed)
			if err != nil {
				t.Fatal(err)
			}
			return grant
		}, false},
		{"wrong swarm", func(t *testing.T, issuer *trackerGrantIssuer, _ string) string {
			grant, _, err := issuer.issueGrant(enrollmentID, strings.Repeat("c", 64), trackerGrantActionDownload)
			if err != nil {
				t.Fatal(err)
			}
			return grant
		}, true},
		{"wrong signer", func(t *testing.T, _ *trackerGrantIssuer, hash string) string {
			other := testTrackerGrantIssuer(t)
			other.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
			grant, _, err := other.issueGrant(enrollmentID, hash, trackerGrantActionDownload)
			if err != nil {
				t.Fatal(err)
			}
			return grant
		}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, issuer, cfg, _ := newWebSeedFixture(t, "content")
			beforeReads := repository.getReads
			grant := test.grant(t, issuer, repository.objects[0].InfoHashV2)
			if err := os.RemoveAll(cfg.root); err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newHandlerWithWebSeed(nil, issuer, cfg, repository).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/web-seed/v1/media/"+activeMediaID+"/"+grant, nil))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
			gotCatalogRead := repository.getReads != beforeReads
			if gotCatalogRead != test.wantCatalogRead {
				t.Fatalf("catalog read = %v, want %v", gotCatalogRead, test.wantCatalogRead)
			}
		})
	}
}

// libtorrent loads a non-piece-aligned v2 file with a synthetic tail pad file,
// treats the torrent as multi-file, and requests <web_seed_url>/<torrent name>.
func TestWebSeedServesLibtorrentExactMediaIDSuffixOnly(t *testing.T) {
	const content = "0123456789abcdefghijklmnopqrstuvwxyz"
	repository, issuer, cfg, grant := newWebSeedFixture(t, content)
	base := "/api/web-seed/v1/media/" + activeMediaID + "/" + grant
	request := httptest.NewRequest(http.MethodGet, base+"/"+activeMediaID, nil)
	request.Header.Set("Range", "bytes=3-7")
	response := httptest.NewRecorder()
	newHandlerWithWebSeed(nil, issuer, cfg, repository).ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "34567" || response.Header().Get("Content-Range") != "bytes 3-7/36" {
		t.Fatalf("status=%d range=%q body=%q", response.Code, response.Header().Get("Content-Range"), response.Body.String())
	}

	for _, path := range []string{
		base + "/" + strings.Replace(activeMediaID, "a", "b", 1),
		base + "/" + activeMediaID + ".torrent",
		base + "/" + activeMediaID + "/" + activeMediaID,
		base + "/",
	} {
		t.Run(path, func(t *testing.T) {
			before := repository.getReads
			response := httptest.NewRecorder()
			newHandlerWithWebSeed(nil, issuer, cfg, repository).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusNotFound || repository.getReads != before {
				t.Fatalf("status=%d catalog_reads=%d body=%q", response.Code, repository.getReads-before, response.Body.String())
			}
		})
	}
}

func TestWebSeedInvalidIDsAndArbitraryPathsCannotReadFiles(t *testing.T) {
	repository, issuer, cfg, grant := newWebSeedFixture(t, "secret content")
	for _, path := range []string{
		"/api/web-seed/v1/media/not-an-id/" + grant,
		"/api/web-seed/v1/media/" + activeMediaID + "/" + grant + "/extra",
		"/api/web-seed/v1/media/" + activeMediaID,
		"/api/web-seed/v1/media/" + activeMediaID + "/" + grant + "?download=true",
		"/api/web-seed/v1/media/../../etc/passwd/" + grant,
	} {
		t.Run(path, func(t *testing.T) {
			before := repository.getReads
			response := httptest.NewRecorder()
			newHandlerWithWebSeed(nil, issuer, cfg, repository).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code == http.StatusOK || response.Body.String() == "secret content" {
				t.Fatalf("path resolved: status=%d body=%q", response.Code, response.Body.String())
			}
			if strings.Contains(path, "not-an-id") && repository.getReads != before {
				t.Fatal("malformed ID read repository")
			}
		})
	}
}

func TestWebSeedRejectsMissingInactiveAndUnsafeFilesystemObjects(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*testing.T, *mediaRepository, *webSeedConfig)
		wantStatus int
	}{
		{"missing catalog object", func(_ *testing.T, repository *mediaRepository, _ *webSeedConfig) {
			repository.objects = repository.objects[1:]
		}, http.StatusNotFound},
		{"removed object", func(_ *testing.T, repository *mediaRepository, _ *webSeedConfig) {
			repository.objects[0].State = "REMOVED"
		}, http.StatusNotFound},
		{"missing file", func(t *testing.T, _ *mediaRepository, cfg *webSeedConfig) {
			if err := os.Remove(filepath.Join(cfg.root, activeMediaID)); err != nil {
				t.Fatal(err)
			}
		}, http.StatusNotFound},
		{"size mismatch", func(_ *testing.T, repository *mediaRepository, _ *webSeedConfig) { repository.objects[0].Size++ }, http.StatusServiceUnavailable},
		{"root unavailable", func(t *testing.T, _ *mediaRepository, cfg *webSeedConfig) {
			if err := os.RemoveAll(cfg.root); err != nil {
				t.Fatal(err)
			}
		}, http.StatusServiceUnavailable},
		{"symlink", func(t *testing.T, _ *mediaRepository, cfg *webSeedConfig) {
			path := filepath.Join(cfg.root, activeMediaID)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("content"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		}, http.StatusServiceUnavailable},
		{"directory", func(t *testing.T, _ *mediaRepository, cfg *webSeedConfig) {
			path := filepath.Join(cfg.root, activeMediaID)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, issuer, cfg, grant := newWebSeedFixture(t, "content")
			test.mutate(t, repository, cfg)
			response := httptest.NewRecorder()
			newHandlerWithWebSeed(nil, issuer, cfg, repository).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/web-seed/v1/media/"+activeMediaID+"/"+grant, nil))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %q", response.Code, response.Body.String())
			}
		})
	}
}

func TestWebSeedRouteIsAbsentWhenDisabled(t *testing.T) {
	repository, _, _, grant := newWebSeedFixture(t, "content")
	response := httptest.NewRecorder()
	newHandlerWithTracker(nil, testTrackerGrantIssuer(t), repository).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/web-seed/v1/media/"+activeMediaID+"/"+grant, nil))
	if response.Code != http.StatusNotFound || repository.getReads != 0 {
		t.Fatalf("status = %d, reads = %d", response.Code, repository.getReads)
	}
}

func TestWebSeedVerifierRequiresCanonicalV2Grant(t *testing.T) {
	issuer := testTrackerGrantIssuer(t)
	issuer.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	hash := strings.Repeat("b", 64)
	grant, _, err := issuer.issueGrant(enrollmentID, hash, trackerGrantActionDownload)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(grant, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	signed := func(payload []byte) string {
		return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(issuer.privateKey, payload))
	}
	wrongVersion := append([]byte(nil), payload...)
	wrongVersion[0] = 1
	unknownAction := append([]byte(nil), payload...)
	unknownAction[57] = 0x04
	for _, malformed := range []string{
		grant + "=",
		parts[0] + "=." + parts[1],
		signed(payload[:57]),
		signed(wrongVersion),
		signed(unknownAction),
	} {
		if issuer.verifyGrant(malformed, hash, trackerGrantActionDownload) {
			t.Fatalf("accepted noncanonical grant %q", malformed)
		}
	}
}
