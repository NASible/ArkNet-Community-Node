package transfer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testID = "media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testToken = "0123456789abcdef0123456789abcdef"

func goodStatus() Status {
	return Status{ObjectID: testID, State: Downloading, TotalBytes: 10, Mock: true}
}
func TestAddWireContract(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/transfer/v1/transfers" {
			t.Errorf("route %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("Content-Type") != "application/json" {
			t.Error("headers")
		}
		if r.URL.RawQuery != "" || r.Header.Get("Cookie") != "" {
			t.Error("credential leak")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body) != 6 || body["object_id"] != testID || body["size"] != float64(10) || body["info_hash_v2"] != strings.Repeat("a", 64) || body["tracker_url"] != "https://tracker.example/announce/grant" || body["web_seed_url"] != "https://core.example/api/web-seed/v1/media/"+testID+"/grant" || body["torrent_url"] != "https://core.example/api/torrent/v1/media/"+testID+"/grant" {
			t.Errorf("unexpected request %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(goodStatus())
	}))
	defer s.Close()
	var svc Service = New(s.URL, testToken)
	got, err := svc.Add(context.Background(), Request{ObjectID: testID, Size: 10, InfoHashV2: strings.Repeat("a", 64), TrackerURL: "https://tracker.example/announce/grant", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + testID + "/grant", TorrentURL: "https://core.example/api/torrent/v1/media/" + testID + "/grant"})
	if err != nil || got != goodStatus() {
		t.Fatalf("got %#v %v", got, err)
	}
}

func TestRefreshWireContract(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/transfer/v1/transfers" {
			t.Fatalf("route %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body) != 6 || body["object_id"] != testID || body["size"] != float64(10) || body["info_hash_v2"] != strings.Repeat("a", 64) || body["tracker_url"] != "https://tracker.example/announce/new" || body["web_seed_url"] != "https://core.example/api/web-seed/v1/media/"+testID+"/new" || body["torrent_url"] != "https://core.example/api/torrent/v1/media/"+testID+"/new" {
			t.Fatalf("body %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		status := goodStatus()
		status.Mock = false
		_ = json.NewEncoder(w).Encode(status)
	}))
	defer s.Close()
	request := Request{ObjectID: testID, Size: 10, InfoHashV2: strings.Repeat("a", 64), TrackerURL: "https://tracker.example/announce/new", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + testID + "/new", TorrentURL: "https://core.example/api/torrent/v1/media/" + testID + "/new"}
	status, err := New(s.URL, testToken).Refresh(context.Background(), request)
	if err != nil || status.Mock {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
