package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const seedTracker = "https://tracker.example/announce/grant.sig"
const seedTorrent = "https://core.example/api/torrent/v1/media/" + testID + "/grant.sig"

func goodSeed() SeedRequest {
	return SeedRequest{ObjectID: testID, Size: 4096, InfoHashV2: strings.Repeat("a", 64), TrackerURL: seedTracker, TorrentURL: seedTorrent}
}

// seedServer answers every request with the given status and body, recording calls.
func seedServer(t *testing.T, code int, body string, check func(*http.Request, []byte)) (*Client, *int) {
	t.Helper()
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Error("missing local bearer token")
		}
		if check != nil {
			check(r, data)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return New(s.URL, testToken), &calls
}

func TestSeedWireContractSendsExactlyFiveFieldsAndNoPath(t *testing.T) {
	client, calls := seedServer(t, 200, `{"object_id":"`+testID+`","state":"CHECKING"}`, func(r *http.Request, data []byte) {
		if r.Method != "POST" || r.URL.Path != "/api/transfer/v1/seeds" || r.URL.RawQuery != "" {
			t.Errorf("route %s %s", r.Method, r.URL)
		}
		var body map[string]any
		if err := json.Unmarshal(data, &body); err != nil {
			t.Error(err)
		}
		if len(body) != 5 || body["object_id"] != testID || body["size"] != float64(4096) || body["info_hash_v2"] != strings.Repeat("a", 64) ||
			body["tracker_url"] != seedTracker || body["torrent_url"] != seedTorrent {
			t.Errorf("unexpected request %#v", body)
		}
	})
	var seeder Seeder = client
	got, err := seeder.Seed(context.Background(), goodSeed())
	if err != nil || got != (SeedStatus{ObjectID: testID, State: SeedChecking}) || *calls != 1 {
		t.Fatalf("got %#v %v calls=%d", got, err, *calls)
	}
}

func TestSeedAuthenticationAndErrorMapping(t *testing.T) {
	for _, c := range []struct {
		code int
		body string
		want error
	}{
		{401, `{"error":"unauthorized"}`, ErrUnauthorized},
		{404, `{"error":"not_found"}`, ErrNotFound},
		{409, `{"error":"conflict"}`, ErrConflict},
		{400, `{"error":"invalid_request"}`, ErrInvalidRequest},
		{503, `{"error":"unavailable"}`, ErrUnavailable},
		{401, `{"error":"unauthorized","detail":"x"}`, ErrInvalidResponse},
	} {
		client, _ := seedServer(t, c.code, c.body, nil)
		if _, err := client.Seed(context.Background(), goodSeed()); !errors.Is(err, c.want) {
			t.Fatalf("seed %d %s: %v", c.code, c.body, err)
		}
		if _, err := client.Unseed(context.Background(), testID); !errors.Is(err, c.want) {
			t.Fatalf("unseed %d %s: %v", c.code, c.body, err)
		}
		if _, err := client.ListSeeds(context.Background()); !errors.Is(err, c.want) {
			t.Fatalf("list %d %s: %v", c.code, c.body, err)
		}
	}
	if _, err := New("http://127.0.0.1:1", "short").ListSeeds(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid local token must fail closed: %v", err)
	}
}

func TestSeedRejectsInvalidIDsAndURLsBeforeSending(t *testing.T) {
	client, calls := seedServer(t, 200, `{}`, nil)
	other := "media_" + strings.Repeat("c", 64)
	for _, mutate := range []func(*SeedRequest){
		func(r *SeedRequest) { r.ObjectID = "media_" + strings.Repeat("A", 64) },
		func(r *SeedRequest) { r.ObjectID = "../" + testID },
		func(r *SeedRequest) { r.Size = -1 },
		func(r *SeedRequest) { r.InfoHashV2 = "" },
		func(r *SeedRequest) { r.InfoHashV2 = strings.Repeat("A", 64) },
		func(r *SeedRequest) { r.TrackerURL = "" },
		func(r *SeedRequest) { r.TrackerURL = seedTracker + "?x=1" },
		func(r *SeedRequest) { r.TrackerURL = "ftp://tracker.example/announce/g" },
		func(r *SeedRequest) { r.TorrentURL = "" },
		func(r *SeedRequest) {
			r.TorrentURL = "https://user:pw@core.example/api/torrent/v1/media/" + testID + "/g"
		},
		func(r *SeedRequest) { r.TorrentURL = strings.Replace(seedTorrent, testID, other, 1) },
		func(r *SeedRequest) { r.TorrentURL = "https://core.example/api/web-seed/v1/media/" + testID + "/g" },
	} {
		request := goodSeed()
		mutate(&request)
		if _, err := client.Seed(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%#v: %v", request, err)
		}
	}
	for _, id := range []string{"", "media_bad", "media_" + strings.Repeat("A", 64), testID + "/x"} {
		if _, err := client.Unseed(context.Background(), id); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("unseed %q: %v", id, err)
		}
	}
	if *calls != 0 {
		t.Fatalf("invalid requests reached the service: %d", *calls)
	}
}

func TestSeedResponsesRejectUnknownMissingOrInvalidFields(t *testing.T) {
	for _, body := range []string{
		`{"object_id":"` + testID + `","state":"CHECKING","tracker_url":"https://tracker.example/announce/x"}`,
		`{"object_id":"` + testID + `","state":"CHECKING","uploaded_bytes":0}`,
		`{"object_id":"` + testID + `"}`,
		`{"object_id":"` + testID + `","state":null}`,
		`{"object_id":"` + testID + `","state":"DOWNLOADING"}`,
		`{"object_id":"` + testID + `","state":"REMOVED"}`,
		`{"object_id":"media_` + strings.Repeat("c", 64) + `","state":"SEEDING"}`,
		`{"object_id":"` + testID + `","state":"SEEDING","state":"SEEDING"}`,
	} {
		client, _ := seedServer(t, 200, body, nil)
		if _, err := client.Seed(context.Background(), goodSeed()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	for _, body := range []string{
		`{"seeds":null}`, `{}`, `{"seeds":[],"transfers":[]}`,
		`{"seeds":[{"object_id":"` + testID + `","state":"SEEDING","total_bytes":1}]}`,
		`{"seeds":[{"object_id":"` + testID + `","state":"SEEDING"},{"object_id":"` + testID + `","state":"CHECKING"}]}`,
		`{"seeds":[{"object_id":"` + testID + `","state":"REMOVED"}]}`,
	} {
		client, _ := seedServer(t, 200, body, nil)
		if _, err := client.ListSeeds(context.Background()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("%s: %v", body, err)
		}
	}
}

func TestListAndDeleteSeeds(t *testing.T) {
	other := "media_" + strings.Repeat("c", 64)
	list, _ := seedServer(t, 200, `{"seeds":[{"object_id":"`+testID+`","state":"SEEDING"},{"object_id":"`+other+`","state":"VERIFICATION_FAILED"}]}`,
		func(r *http.Request, data []byte) {
			if r.Method != "GET" || r.URL.Path != "/api/transfer/v1/seeds" || len(data) != 0 {
				t.Errorf("list route %s %s body=%d", r.Method, r.URL.Path, len(data))
			}
		})
	got, err := list.ListSeeds(context.Background())
	if err != nil || len(got) != 2 || got[0] != (SeedStatus{testID, SeedSeeding}) || got[1] != (SeedStatus{other, SeedVerificationFailed}) {
		t.Fatalf("list %#v %v", got, err)
	}
	empty, _ := seedServer(t, 200, `{"seeds":[]}`, nil)
	if got, err := empty.ListSeeds(context.Background()); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty list %#v %v", got, err)
	}
	deleted, _ := seedServer(t, 200, `{"object_id":"`+testID+`","state":"REMOVED"}`, func(r *http.Request, data []byte) {
		if r.Method != "DELETE" || r.URL.Path != "/api/transfer/v1/seeds/"+testID || r.URL.RawQuery != "" || len(data) != 0 {
			t.Errorf("delete route %s %s body=%d", r.Method, r.URL, len(data))
		}
	})
	if got, err := deleted.Unseed(context.Background(), testID); err != nil || got.State != SeedRemoved {
		t.Fatalf("unseed %#v %v", got, err)
	}
	for _, body := range []string{
		`{"object_id":"` + testID + `","state":"SEEDING"}`,
		`{"object_id":"` + other + `","state":"REMOVED"}`,
	} {
		wrong, _ := seedServer(t, 200, body, nil)
		if _, err := wrong.Unseed(context.Background(), testID); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("%s: %v", body, err)
		}
	}
}
