package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTrackerGrantCanonicalPayloadAndSignature(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse("https://tracker.example.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	infoHash := strings.Repeat("ab", 32)
	issuer := &trackerGrantIssuer{
		announceBase: base,
		privateKey:   privateKey,
		now:          func() time.Time { return now },
	}

	trackerURL, expiresAt, err := issuer.issue(enrollmentID, infoHash, trackerGrantActionDownload|trackerGrantActionSeed)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(5 * time.Minute); !expiresAt.Equal(want) {
		t.Fatalf("expires_at = %s, want %s", expiresAt, want)
	}
	const prefix = "https://tracker.example.test:8443/announce/"
	if !strings.HasPrefix(trackerURL, prefix) {
		t.Fatalf("tracker URL = %q", trackerURL)
	}
	parts := strings.Split(strings.TrimPrefix(trackerURL, prefix), ".")
	if len(parts) != 2 || strings.Contains(parts[0], "=") || strings.Contains(parts[1], "=") {
		t.Fatalf("grant is not two raw URL-base64 segments: %q", trackerURL)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) != 58 || payload[0] != 2 {
		t.Fatalf("payload length/version = %d/%d", len(payload), payload[0])
	}
	if got := int64(binary.BigEndian.Uint64(payload[1:9])); got != expiresAt.Unix() {
		t.Fatalf("expiry = %d, want %d", got, expiresAt.Unix())
	}
	decodedHash, _ := hex.DecodeString(infoHash)
	subjectMessage := append([]byte("arknet-tracker-subject-v1\x00"+enrollmentID+"\x00"), decodedHash...)
	wantSubject := sha256.Sum256(ed25519.Sign(privateKey, subjectMessage))
	if !bytes.Equal(payload[9:25], wantSubject[:16]) {
		t.Fatalf("subject = %x, want %x", payload[9:25], wantSubject[:16])
	}
	if !bytes.Equal(payload[25:57], decodedHash) {
		t.Fatalf("info hash = %x", payload[25:57])
	}
	if payload[57] != trackerGrantActionDownload|trackerGrantActionSeed {
		t.Fatalf("action mask = %#x", payload[57])
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		t.Fatal("signature does not verify over canonical payload")
	}
}

func TestTrackerGrantSubjectsAreStableAndPairwise(t *testing.T) {
	issuer := testTrackerGrantIssuer(t)
	issuer.now = func() time.Time { return time.Unix(2_000_000_000, 0) }
	firstHash := strings.Repeat("a", 64)
	secondHash := strings.Repeat("b", 64)

	firstURL, _, err := issuer.issue(enrollmentID, firstHash, trackerGrantActionDownload|trackerGrantActionSeed)
	if err != nil {
		t.Fatal(err)
	}
	refreshedURL, _, err := issuer.issue(enrollmentID, firstHash, trackerGrantActionDownload|trackerGrantActionSeed)
	if err != nil {
		t.Fatal(err)
	}
	otherNodeURL, _, err := issuer.issue("node_"+strings.Repeat("2", 64), firstHash, trackerGrantActionDownload|trackerGrantActionSeed)
	if err != nil {
		t.Fatal(err)
	}
	otherSwarmURL, _, err := issuer.issue(enrollmentID, secondHash, trackerGrantActionDownload|trackerGrantActionSeed)
	if err != nil {
		t.Fatal(err)
	}

	firstSubject := trackerGrantSubjectFromURL(t, firstURL)
	if refreshed := trackerGrantSubjectFromURL(t, refreshedURL); refreshed != firstSubject {
		t.Fatalf("refreshed subject = %x, want %x", refreshed, firstSubject)
	}
	if otherNode := trackerGrantSubjectFromURL(t, otherNodeURL); otherNode == firstSubject {
		t.Fatal("different nodes received the same swarm subject")
	}
	if otherSwarm := trackerGrantSubjectFromURL(t, otherSwarmURL); otherSwarm == firstSubject {
		t.Fatal("one node received the same subject across swarms")
	}
}

func trackerGrantSubjectFromURL(t *testing.T, trackerURL string) [16]byte {
	t.Helper()
	parsed, err := url.Parse(trackerURL)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(parsed.Path, "/announce/")
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatalf("invalid grant URL %q", trackerURL)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payload) != 58 {
		t.Fatalf("invalid grant payload in %q", trackerURL)
	}
	var subject [16]byte
	copy(subject[:], payload[9:25])
	return subject
}

func TestTrackerGrantRejectsNonCanonicalInfoHashV2(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://tracker.example.test")
	issuer := &trackerGrantIssuer{
		announceBase: base,
		privateKey:   privateKey,
		now:          time.Now,
	}
	for _, infoHash := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if _, _, err := issuer.issue(enrollmentID, infoHash, trackerGrantActionDownload); err == nil {
			t.Fatalf("issue accepted invalid info hash %q", infoHash)
		}
	}
}

func TestTrackerGrantIssuerAcceptsOnlyNonzeroKnownActionMasks(t *testing.T) {
	issuer := testTrackerGrantIssuer(t)
	infoHash := strings.Repeat("c", 64)
	for _, mask := range []byte{trackerGrantActionDownload, trackerGrantActionSeed, trackerGrantActionDownload | trackerGrantActionSeed} {
		trackerURL, _, err := issuer.issue(enrollmentID, infoHash, mask)
		if err != nil {
			t.Fatalf("issue rejected valid action mask %#x: %v", mask, err)
		}
		payload := trackerGrantPayloadFromURL(t, trackerURL)
		if payload[57] != mask {
			t.Fatalf("encoded action mask = %#x, want %#x", payload[57], mask)
		}
	}
	for _, mask := range []byte{0, 0x04, 0x05, 0xff} {
		if _, _, err := issuer.issue(enrollmentID, infoHash, mask); err == nil {
			t.Fatalf("issue accepted invalid action mask %#x", mask)
		}
	}
}

func trackerGrantPayloadFromURL(t *testing.T, trackerURL string) []byte {
	t.Helper()
	parsed, err := url.Parse(trackerURL)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimPrefix(parsed.Path, "/announce/"), ".")
	if len(parts) != 2 {
		t.Fatalf("invalid grant URL %q", trackerURL)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payload) != 58 {
		t.Fatalf("invalid grant payload in %q", trackerURL)
	}
	return payload
}

func TestApprovedNodeCanIssueScopedSwarmAuthorizationToken(t *testing.T) {
	repository, nodePrivateKey := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	issuer := testTrackerGrantIssuer(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	issuer.now = func() time.Time { return now }

	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	request := signedRequest("POST", path, "", enrollmentID, nodePrivateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()

	newHandlerWithTracker(nil, issuer, repository).ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", response.Header())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 3 || body["tracker_url"] == nil || body["expires_at"] == nil || body["actions"] == nil {
		t.Fatalf("response field set = %#v, want exactly tracker_url, expires_at, and actions", body)
	}
	var trackerURL, expiresAt string
	var actions []string
	if json.Unmarshal(body["tracker_url"], &trackerURL) != nil || json.Unmarshal(body["expires_at"], &expiresAt) != nil || json.Unmarshal(body["actions"], &actions) != nil {
		t.Fatalf("invalid response = %s", response.Body)
	}
	if !strings.HasPrefix(trackerURL, "https://tracker.example.test/announce/") || expiresAt != now.Add(trackerGrantLifetime).Format(time.RFC3339) {
		t.Fatalf("response = %#v", body)
	}
	if len(actions) != 1 || actions[0] != "download" {
		t.Fatalf("actions = %#v", actions)
	}
	payload := trackerGrantPayloadFromURL(t, trackerURL)
	if payload[57] != trackerGrantActionDownload {
		t.Fatalf("handler action mask = %#x", payload[57])
	}
}

func TestTrackerGrantExplicitActionsAreOptInAndCanonicalized(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		wantActions []string
		wantMask    byte
	}{
		{"seed only", `{"actions":["seed"]}`, []string{"seed"}, trackerGrantActionSeed},
		{"both in canonical order", `{"actions":["download","seed"]}`, []string{"download", "seed"}, trackerGrantActionDownload | trackerGrantActionSeed},
		{"both in reverse order", `{"actions":["seed","download"]}`, []string{"download", "seed"}, trackerGrantActionDownload | trackerGrantActionSeed},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, privateKey := approvedMediaRepository(t)
			repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
			path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
			request := signedRequest(http.MethodPost, path, test.body, enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
			response := httptest.NewRecorder()

			eligibleSource := &seedingMediaRepository{mediaRepository: repository, eligible: true}
			newHandlerWithTracker(nil, testTrackerGrantIssuer(t), eligibleSource).ServeHTTP(response, request)

			if response.Code != http.StatusOK || repository.getReads != 1 {
				t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body.String())
			}
			var result struct {
				TrackerURL string   `json:"tracker_url"`
				Actions    []string `json:"actions"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(result.Actions) != fmt.Sprint(test.wantActions) {
				t.Fatalf("actions = %#v, want %#v", result.Actions, test.wantActions)
			}
			if got := trackerGrantPayloadFromURL(t, result.TrackerURL)[57]; got != test.wantMask {
				t.Fatalf("action mask = %#x, want %#x", got, test.wantMask)
			}
		})
	}
}

func testTrackerGrantIssuer(t *testing.T) *trackerGrantIssuer {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://tracker.example.test")
	return &trackerGrantIssuer{announceBase: base, privateKey: privateKey, now: time.Now}
}

func TestTrackerGrantAuthenticatesBeforeReadingMedia(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*mediaRepository, *http.Request)
		status int
	}{
		{"unsigned", func(_ *mediaRepository, request *http.Request) { request.Header.Del("X-ArkNet-Node-Signature") }, http.StatusUnauthorized},
		{"wrong signature", func(_ *mediaRepository, request *http.Request) {
			_, wrongKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = signedRequest(request.Method, request.URL.Path, "", enrollmentID, wrongKey, request.Header.Get("X-ArkNet-Node-Timestamp")).Header
		}, http.StatusUnauthorized},
		{"wrong node path", func(_ *mediaRepository, request *http.Request) {
			request.URL.Path = strings.Replace(request.URL.Path, enrollmentID, "node_"+strings.Repeat("2", 64), 1)
		}, http.StatusUnauthorized},
		{"pending and untrusted", func(repository *mediaRepository, _ *http.Request) {
			repository.node.EnrollmentStatus = "PENDING"
			repository.node.TrustStatus = "UNTRUSTED"
		}, http.StatusForbidden},
		{"approved but blocked", func(repository *mediaRepository, _ *http.Request) {
			repository.node.EnrollmentStatus = "APPROVED"
			repository.node.TrustStatus = "BLOCKED"
		}, http.StatusForbidden},
		{"rejected and blocked", func(repository *mediaRepository, _ *http.Request) {
			repository.node.EnrollmentStatus = "REJECTED"
			repository.node.TrustStatus = "BLOCKED"
		}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, privateKey := approvedMediaRepository(t)
			repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
			path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
			request := signedRequest("POST", path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
			test.mutate(repository, request)
			response := httptest.NewRecorder()
			newHandlerWithTracker(nil, testTrackerGrantIssuer(t), repository).ServeHTTP(response, request)
			if response.Code != test.status || repository.getReads != 0 {
				t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body.String())
			}
		})
	}
}

func TestTrackerGrantRejectsQueryBeforeReadingMedia(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants?watch=true"
	request := signedRequest(http.MethodPost, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()
	newHandlerWithTracker(nil, testTrackerGrantIssuer(t), repository).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || repository.getReads != 0 {
		t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body.String())
	}
}

func TestTrackerGrantRejectsInvalidActionBodiesBeforeReadingMedia(t *testing.T) {
	for _, body := range []string{
		`{`,
		`{}`,
		`{"other":[]}`,
		`{"actions":[]}`,
		`{"actions":["download","download"]}`,
		`{"actions":["leech"]}`,
		`{"actions":["download"],"actions":["seed"]}`,
		`{"actions":["download"]} {}`,
		strings.Repeat(" ", 257),
	} {
		t.Run(body, func(t *testing.T) {
			repository, privateKey := approvedMediaRepository(t)
			path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
			request := signedRequest(http.MethodPost, path, body, enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
			response := httptest.NewRecorder()
			newHandlerWithTracker(nil, testTrackerGrantIssuer(t), repository).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || repository.getReads != 0 {
				t.Fatalf("status = %d, reads = %d, response = %s", response.Code, repository.getReads, response.Body.String())
			}
		})
	}
}

func TestTrackerGrantAuthenticatesExactActionBodyBeforeReadingMedia(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	request := signedRequest(http.MethodPost, path, `{"actions":["seed"]}`, enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	tampered := `{"actions":["download"]}`
	request.Body = io.NopCloser(strings.NewReader(tampered))
	request.ContentLength = int64(len(tampered))
	response := httptest.NewRecorder()
	newHandlerWithTracker(nil, testTrackerGrantIssuer(t), repository).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || repository.getReads != 0 {
		t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body.String())
	}
}

func TestTrackerGrantValidatesMediaBeforeIssuing(t *testing.T) {
	for _, test := range []struct {
		name       string
		mediaID    string
		mutate     func(*mediaRepository)
		wantStatus int
		wantReads  int
	}{
		{"malformed ID", "bad", func(*mediaRepository) {}, http.StatusBadRequest, 0},
		{"missing object", "media_1111111111111111111111111111111111111111111111111111111111111111", func(*mediaRepository) {}, http.StatusNotFound, 1},
		{"removed object", removedMediaID, func(*mediaRepository) {}, http.StatusConflict, 1},
		{"missing hash", activeMediaID, func(*mediaRepository) {}, http.StatusConflict, 1},
		{"uppercase hash", activeMediaID, func(repository *mediaRepository) { repository.objects[0].InfoHashV2 = strings.Repeat("A", 64) }, http.StatusConflict, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, privateKey := approvedMediaRepository(t)
			test.mutate(repository)
			path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + test.mediaID + "/tracker-grants"
			response := httptest.NewRecorder()
			newHandlerWithTracker(nil, testTrackerGrantIssuer(t), repository).ServeHTTP(response, signedRequest("POST", path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix())))
			if response.Code != test.wantStatus || repository.getReads != test.wantReads {
				t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body.String())
			}
		})
	}
}

func TestTrackerGrantReturnsUnavailableWhenTrackerIsDisabled(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	repository.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/tracker-grants"
	response := httptest.NewRecorder()
	newHandler(nil, repository).ServeHTTP(response, signedRequest("POST", path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix())))
	if response.Code != http.StatusServiceUnavailable || repository.getReads != 1 {
		t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body.String())
	}
}
