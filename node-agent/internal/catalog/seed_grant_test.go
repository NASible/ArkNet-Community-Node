package catalog

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
)

const seedObjectID = "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func seedIdentity(url string) enrollment.Identity {
	return enrollment.Identity{NodeID: catalogNodeID, CoreURL: url, Status: "APPROVED", PrivateSeed: make([]byte, ed25519.SeedSize)}
}

func seedGrantBody(fields string) string {
	return fmt.Sprintf(`{"tracker_url":"https://tracker.example/announce/token","torrent_url":"https://core.example/api/torrent/v1/media/%s/token","expires_at":%q,"actions":["seed"]%s}`,
		seedObjectID, time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339), fields)
}

func TestSeedGrantSignsExactSeedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/federation/v1/nodes/"+catalogNodeID+"/media/"+seedObjectID+"/tracker-grants" ||
			r.Header.Get("X-ArkNet-Node-Signature") == "" || string(body) != `{"actions":["seed"]}` || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected seed grant request: %s %s %q", r.Method, r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, seedGrantBody(""))
	}))
	defer server.Close()
	grant, err := SeedGrant(context.Background(), seedIdentity(server.URL), seedObjectID)
	if err != nil || grant.TrackerURL != "https://tracker.example/announce/token" || !strings.Contains(grant.TorrentURL, seedObjectID) ||
		grant.WebSeedURL != "" || len(grant.Actions) != 1 || grant.Actions[0] != "seed" || grant.ExpiresAt.IsZero() {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
}

func TestSeedGrantRejectsWebSeedDownloadOrMissingMetadata(t *testing.T) {
	future := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	for _, body := range []string{
		seedGrantBody(`,"web_seed_url":"https://core.example/api/web-seed/v1/media/` + seedObjectID + `/token"`),
		seedGrantBody(`,"web_seed_url":""`),
		strings.Replace(seedGrantBody(""), `["seed"]`, `["download"]`, 1),
		strings.Replace(seedGrantBody(""), `["seed"]`, `["download","seed"]`, 1),
		strings.Replace(seedGrantBody(""), `["seed"]`, `[]`, 1),
		`{"tracker_url":"https://tracker.example/announce/token","expires_at":"` + future + `","actions":["seed"]}`,
		strings.Replace(seedGrantBody(""), seedObjectID+"/token", "media_"+strings.Repeat("c", 64)+"/token", 1),
		seedGrantBody(`,"extra":true`),
		strings.Replace(seedGrantBody(""), "https://tracker.example", "https://user:pw@tracker.example", 1),
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		_, err := SeedGrant(context.Background(), seedIdentity(server.URL), seedObjectID)
		server.Close()
		if !errors.Is(err, ErrRequestFailed) || errors.Is(err, ErrGrantRefused) {
			t.Fatalf("accepted or misclassified %s: %v", body, err)
		}
	}
}

func TestSeedGrantDistinguishesCoreRefusalFromFailure(t *testing.T) {
	for code, refused := range map[int]bool{401: true, 403: true, 404: false, 500: false, 503: false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"node is not an eligible source"}`)
		}))
		_, err := SeedGrant(context.Background(), seedIdentity(server.URL), seedObjectID)
		server.Close()
		if err == nil || errors.Is(err, ErrGrantRefused) != refused {
			t.Fatalf("status %d: err=%v, want refused=%v", code, err, refused)
		}
	}
	unreachable := seedIdentity("http://127.0.0.1:1")
	if _, err := SeedGrant(context.Background(), unreachable, seedObjectID); err == nil || errors.Is(err, ErrGrantRefused) {
		t.Fatalf("an unreachable Core is not a refusal: %v", err)
	}
	notApproved := seedIdentity("https://core.example")
	notApproved.Status = "REVOKED"
	if _, err := SeedGrant(context.Background(), notApproved, seedObjectID); err == nil {
		t.Fatal("unapproved identity requested a seed grant")
	}
}
