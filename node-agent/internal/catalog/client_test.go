package catalog

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
)

const catalogNodeID = "node_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestFetchReturnsActiveAndRemovedCatalogObjects(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	publicKey := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	removedID := "media_abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/federation/v1/nodes/"+catalogNodeID+"/media" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		timestamp := request.Header.Get("X-ArkNet-Node-Timestamp")
		signature, err := base64.StdEncoding.Strict().DecodeString(request.Header.Get("X-ArkNet-Node-Signature"))
		if err != nil {
			t.Errorf("invalid signature encoding: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
			t.Errorf("invalid timestamp: %q", timestamp)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		message := fmt.Sprintf("ARKNET-NODE-V1\n%s\n%s\n%x\n%s\n%s", request.Method, request.URL.EscapedPath(), sha256.Sum256(nil), timestamp, catalogNodeID)
		if !ed25519.Verify(publicKey, []byte(message), signature) {
			t.Error("request signature did not verify")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"objects": []map[string]any{
				{"id": "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "content_type": "VIDEO", "title": "Movie", "size": 1024, "state": "ACTIVE", "info_hash_v2": "", "created_at": "2026-10-14T04:00:00Z", "updated_at": "2026-10-14T04:00:00Z"},
				{"id": removedID, "content_type": "AUDIO", "title": "Album", "size": 2048, "state": "REMOVED", "info_hash_v2": strings.Repeat("a", 64), "created_at": "2026-10-14T04:00:00Z", "updated_at": "2026-10-14T04:01:00Z"},
			},
			"page": 1, "limit": 100,
		})
	}))
	defer server.Close()

	objects, err := Fetch(context.Background(), enrollment.Identity{NodeID: catalogNodeID, CoreURL: server.URL, Status: "APPROVED", PrivateSeed: seed})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 || objects[0].State != Active || objects[1].ID != removedID || objects[1].State != Removed {
		t.Fatalf("objects = %+v", objects)
	}
}

func TestFetchRejectsMalformedCatalog(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"objects":[{"id":"/srv/media/movie.mkv","content_type":"VIDEO","title":"Movie","size":1,"state":"ACTIVE","info_hash_v2":"","created_at":"2026-10-14T04:00:00Z","updated_at":"2026-10-14T04:00:00Z"}],"page":1,"limit":100}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := Fetch(ctx, enrollment.Identity{NodeID: catalogNodeID, CoreURL: server.URL, Status: "APPROVED", PrivateSeed: seed}); err != ErrRequestFailed {
		t.Fatalf("error = %v, want ErrRequestFailed", err)
	}
}

func TestGrantSignsRequestAndReturnsTransferURLs(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	objectID := "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/federation/v1/nodes/"+catalogNodeID+"/media/"+objectID+"/tracker-grants" || r.Header.Get("X-ArkNet-Node-Signature") == "" {
			t.Fatalf("unsigned or unexpected grant request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"tracker_url":"https://tracker.example/announce/token","web_seed_url":"https://core.example/api/web-seed/v1/media/%s/token","torrent_url":"https://core.example/api/torrent/v1/media/%s/token","expires_at":%q,"actions":["download"]}`, objectID, objectID, time.Now().Add(5*time.Minute).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	grant, err := Grant(context.Background(), enrollment.Identity{NodeID: catalogNodeID, CoreURL: server.URL, Status: "APPROVED", PrivateSeed: seed}, objectID)
	if err != nil || grant.TrackerURL != "https://tracker.example/announce/token" || !strings.Contains(grant.WebSeedURL, objectID) || !strings.Contains(grant.TorrentURL, objectID) || grant.ExpiresAt.IsZero() || len(grant.Actions) != 1 || grant.Actions[0] != "download" {
		t.Fatalf("grant=%+v err=%v", grant, err)
	}
}

func TestGrantRejectsMalformedOrCredentialedURLs(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	objectID := "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	future := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	for _, body := range []string{
		`{"tracker_url":"https://user:secret@tracker.example/announce/token","expires_at":"` + future + `","actions":["download"]}`,
		`{"tracker_url":"https://tracker.example/announce/token","web_seed_url":"https://core.example/wrong","expires_at":"` + future + `","actions":["download"]}`,
		`{"tracker_url":"https://tracker.example/announce/token","expires_at":"` + future + `","actions":["download"],"extra":true}`,
		`{"tracker_url":"https://tracker.example/announce/token","expires_at":"` + future + `","actions":["seed"]}`,
		`{"tracker_url":"https://tracker.example/announce/token","expires_at":"2000-01-01T00:00:00Z","actions":["download"]}`,
	} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		_, err := Grant(context.Background(), enrollment.Identity{NodeID: catalogNodeID, CoreURL: s.URL, Status: "APPROVED", PrivateSeed: seed}, objectID)
		s.Close()
		if err != ErrRequestFailed {
			t.Fatalf("accepted %s: %v", body, err)
		}
	}
}

func TestGrantAcceptsHTTPOnlyFromHTTPDevelopmentCore(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	objectID := "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tracker_url":"http://arknet-tracker:6969/announce/token","web_seed_url":"http://arknet-core:8080/api/web-seed/v1/media/%s/token","torrent_url":"http://arknet-core:8080/api/torrent/v1/media/%s/token","expires_at":%q,"actions":["download"]}`, objectID, objectID, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	if _, err := Grant(context.Background(), enrollment.Identity{NodeID: catalogNodeID, CoreURL: server.URL, Status: "APPROVED", PrivateSeed: seed}, objectID); err != nil {
		t.Fatal(err)
	}
}

func TestReportReplicaSignsExactCoreContract(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	objectID := "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	hash := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/federation/v1/nodes/"+catalogNodeID+"/media/"+objectID+"/replicas" || r.Header.Get("X-ArkNet-Node-Signature") == "" {
			t.Fatalf("unsigned or unexpected replica request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]json.RawMessage
		err := json.NewDecoder(r.Body).Decode(&body)
		if err != nil || len(body) != 2 || string(body["size"]) != "1024" || string(body["info_hash_v2"]) != `"`+hash+`"` {
			t.Fatalf("replica body = %#v err=%v", body, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := ReportReplica(context.Background(), enrollment.Identity{NodeID: catalogNodeID, CoreURL: server.URL, Status: "APPROVED", PrivateSeed: seed}, objectID, 1024, hash); err != nil {
		t.Fatal(err)
	}
}
