package main

import (
	"arknet/core/internal/nodes"
	"context"
	"crypto/ed25519"
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

// seedingMediaRepository adds Core's authoritative source-eligibility answer.
type seedingMediaRepository struct {
	*mediaRepository
	eligible bool
	asked    []string
}

func (r *seedingMediaRepository) MediaSourceEligible(_ context.Context, nodeID, mediaID string) (bool, error) {
	r.asked = append(r.asked, nodeID+"|"+mediaID)
	return r.eligible, nil
}

func seedGrantRequest(t *testing.T, repository nodes.EnrollmentRepository, key ed25519.PrivateKey, body string, webSeed *webSeedConfig, issuer *trackerGrantIssuer) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	request := signedRequest(http.MethodPost, path, body, enrollmentID, key, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()
	newHandlerWithWebSeed(nil, issuer, webSeed, repository).ServeHTTP(response, request)
	return response
}

func TestSeedGrantRequiresCoreSourceEligibility(t *testing.T) {
	for name, build := range map[string]func(*mediaRepository) nodes.EnrollmentRepository{
		"no eligibility authority": func(r *mediaRepository) nodes.EnrollmentRepository { return r },
		"not an eligible source": func(r *mediaRepository) nodes.EnrollmentRepository {
			return &seedingMediaRepository{mediaRepository: r}
		},
	} {
		for _, body := range []string{`{"actions":["seed"]}`, `{"actions":["download","seed"]}`} {
			t.Run(name+" "+body, func(t *testing.T) {
				repository, privateKey := approvedMediaRepository(t)
				repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
				response := seedGrantRequest(t, build(repository), privateKey, body, nil, testTrackerGrantIssuer(t))
				if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "/announce/") {
					t.Fatalf("status = %d body = %s", response.Code, response.Body)
				}
			})
		}
	}
}

func TestEligibleSourceReceivesSeedGrantWithMetadataButNoWebSeed(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	eligible := &seedingMediaRepository{mediaRepository: repository, eligible: true}
	origin, _ := url.Parse("https://core.example.test")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, activeMediaID+".torrent"), []byte("d4:infod6:lengthi1eee"), 0o600); err != nil {
		t.Fatal(err)
	}
	issuer := testTrackerGrantIssuer(t)
	response := seedGrantRequest(t, eligible, privateKey, `{"actions":["seed"]}`, &webSeedConfig{origin: origin, root: root}, issuer)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body)
	}
	if len(eligible.asked) != 1 || eligible.asked[0] != enrollmentID+"|"+activeMediaID {
		t.Fatalf("eligibility asked = %v", eligible.asked)
	}
	var grant struct {
		TrackerURL string   `json:"tracker_url"`
		Actions    []string `json:"actions"`
		WebSeedURL string   `json:"web_seed_url"`
		TorrentURL string   `json:"torrent_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(grant.Actions) != "[seed]" || grant.WebSeedURL != "" || !strings.HasPrefix(grant.TorrentURL, "https://core.example.test/api/torrent/v1/media/"+activeMediaID+"/") {
		t.Fatalf("grant = %+v", grant)
	}
	// The seed grant opens only metadata; Core's web seed payload stays download-only.
	parsed, _ := url.Parse(grant.TorrentURL)
	token := strings.TrimPrefix(parsed.Path, "/api/torrent/v1/media/"+activeMediaID+"/")
	metadata := httptest.NewRecorder()
	newHandlerWithWebSeed(nil, issuer, &webSeedConfig{origin: origin, root: root}, eligible).ServeHTTP(metadata,
		httptest.NewRequest(http.MethodGet, parsed.Path, nil))
	if metadata.Code != http.StatusOK {
		t.Fatalf("metadata status = %d", metadata.Code)
	}
	payload := httptest.NewRecorder()
	newHandlerWithWebSeed(nil, issuer, &webSeedConfig{origin: origin, root: root}, eligible).ServeHTTP(payload,
		httptest.NewRequest(http.MethodGet, "/api/web-seed/v1/media/"+activeMediaID+"/"+token, nil))
	if payload.Code != http.StatusForbidden {
		t.Fatalf("seed grant read web-seed payload: %d", payload.Code)
	}
}
