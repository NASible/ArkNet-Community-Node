package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"arknet/transfer/engine"
)

const seedsBase = "/api/transfer/v1/seeds"
const seedHash = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// seedRecordingEngine stands in for the libtorrent backend. URL validation
// lives in backend.Client (tested there); this fake rejects any URL other than
// the canonical grant URLs the same way, so the HTTP mapping is exercised.
type seedRecordingEngine struct {
	*engine.Engine
	seeds   map[string]engine.SeedState
	calls   []string
	failAll bool
}

func newSeedEngine() *seedRecordingEngine {
	return &seedRecordingEngine{Engine: engine.New(time.Minute, nil), seeds: map[string]engine.SeedState{}}
}

func (e *seedRecordingEngine) Seed(id string, size int64, hash, tracker, torrent string) (engine.SeedStatus, error) {
	e.calls = append(e.calls, "seed")
	if e.failAll {
		return engine.SeedStatus{}, errors.New("worker unavailable: " + tracker)
	}
	if tracker != trackerURL || torrent != torrentURL {
		return engine.SeedStatus{}, engine.ErrInvalid
	}
	if hash != seedHash || size != 4096 {
		return engine.SeedStatus{}, engine.ErrConflict
	}
	e.seeds[id] = engine.SeedChecking
	return engine.SeedStatus{ObjectID: id, State: engine.SeedChecking}, nil
}

func (e *seedRecordingEngine) Unseed(id string) (engine.SeedStatus, error) {
	e.calls = append(e.calls, "unseed")
	if _, ok := e.seeds[id]; !ok {
		return engine.SeedStatus{}, engine.ErrNotFound
	}
	delete(e.seeds, id)
	return engine.SeedStatus{ObjectID: id, State: engine.SeedRemoved}, nil
}

func (e *seedRecordingEngine) Seeds() ([]engine.SeedStatus, error) {
	e.calls = append(e.calls, "seeds")
	if e.failAll {
		return nil, errors.New("worker unavailable")
	}
	var out []engine.SeedStatus
	for id, state := range e.seeds {
		out = append(out, engine.SeedStatus{ObjectID: id, State: state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out, nil
}

func seedBody(fields map[string]any) string {
	body := map[string]any{"object_id": objectID, "size": 4096, "info_hash_v2": seedHash,
		"tracker_url": trackerURL, "torrent_url": torrentURL}
	for k, v := range fields {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func TestSeedRoutesRequireTheLocalBearerToken(t *testing.T) {
	e := newSeedEngine()
	h := newHandler(testToken, e)
	for _, route := range []struct{ method, path, body string }{
		{http.MethodGet, seedsBase, ""}, {http.MethodPost, seedsBase, seedBody(nil)},
		{http.MethodDelete, seedsBase + "/" + objectID, ""},
	} {
		for _, token := range []string{"", "wrong-token-wrong-token-wrong-tok", testToken + "x"} {
			got := request(t, h, route.method, route.path, route.body, token, 401)
			if got["error"] != "unauthorized" {
				t.Fatalf("%s %s: %v", route.method, route.path, got)
			}
		}
	}
	if len(e.calls) != 0 {
		t.Fatalf("unauthenticated requests reached the engine: %v", e.calls)
	}
}

func TestSeedLifecycleListsOnlyIDAndStateThenDeletes(t *testing.T) {
	e := newSeedEngine()
	h := newHandler(testToken, e)
	empty := request(t, h, http.MethodGet, seedsBase, "", testToken, 200)
	if list, ok := empty["seeds"].([]any); !ok || len(list) != 0 {
		t.Fatalf("empty list must be [], got %#v", empty)
	}
	added := request(t, h, http.MethodPost, seedsBase, seedBody(nil), testToken, 200)
	if len(added) != 2 || added["object_id"] != objectID || added["state"] != "CHECKING" {
		t.Fatalf("seed response %#v", added)
	}
	listed := request(t, h, http.MethodGet, seedsBase, "", testToken, 200)
	items := listed["seeds"].([]any)
	if len(listed) != 1 || len(items) != 1 {
		t.Fatalf("list %#v", listed)
	}
	if item := items[0].(map[string]any); len(item) != 2 || item["object_id"] != objectID || item["state"] != "CHECKING" {
		t.Fatalf("list item must carry only object_id and state: %#v", item)
	}
	removed := request(t, h, http.MethodDelete, seedsBase+"/"+objectID, "", testToken, 200)
	if len(removed) != 2 || removed["state"] != "REMOVED" {
		t.Fatalf("delete %#v", removed)
	}
	if got := request(t, h, http.MethodDelete, seedsBase+"/"+objectID, "", testToken, 404); got["error"] != "not_found" {
		t.Fatalf("second delete %#v", got)
	}
	if list := request(t, h, http.MethodGet, seedsBase, "", testToken, 200)["seeds"].([]any); len(list) != 0 {
		t.Fatalf("seed survived delete: %#v", list)
	}
}

func TestSeedRequestsRejectUnknownFieldsInvalidIDsAndURLs(t *testing.T) {
	e := newSeedEngine()
	h := newHandler(testToken, e)
	bad := []map[string]any{
		{"file_name": objectID + ".mp4"}, {"web_seed_url": "https://core.arknet.test/x"}, {"path": "/srv"},
		{"Object_ID": objectID}, {"object_id": nil}, {"torrent_url": nil}, {"tracker_url": nil},
		{"object_id": "media_" + strings.Repeat("A", 64)}, {"object_id": "../" + objectID}, {"object_id": 7},
		{"size": -1}, {"size": 1.5}, {"size": "4096"}, {"info_hash_v2": strings.Repeat("D", 64)},
		{"info_hash_v2": "abc"}, {"tracker_url": ""}, {"torrent_url": ""}, {"tracker_url": 1},
	}
	for _, fields := range bad {
		if got := request(t, h, http.MethodPost, seedsBase, seedBody(fields), testToken, 400); got["error"] != "invalid_request" {
			t.Fatalf("%v: %#v", fields, got)
		}
	}
	for _, raw := range []string{"", "{}", "null", "[]", seedBody(nil) + "{}", `{"object_id":"a","object_id":"b"}`} {
		request(t, h, http.MethodPost, seedsBase, raw, testToken, 400)
	}
	if len(e.calls) != 0 {
		t.Fatalf("invalid requests reached the engine: %v", e.calls)
	}
	// URL authority is enforced by the engine (backend.Client validators).
	for _, fields := range []map[string]any{
		{"tracker_url": "https://evil.test/announce/YWJj.ZGVm"},
		{"torrent_url": "https://core.arknet.test/api/torrent/v1/media/media_" + strings.Repeat("c", 64) + "/YWJj.ZGVm"},
	} {
		got := request(t, h, http.MethodPost, seedsBase, seedBody(fields), testToken, 400)
		if len(got) != 1 || got["error"] != "invalid_request" {
			t.Fatalf("%v: %#v", fields, got)
		}
	}
	if got := request(t, h, http.MethodPost, seedsBase, seedBody(map[string]any{"size": 4097}), testToken, 409); got["error"] != "conflict" {
		t.Fatalf("conflict %#v", got)
	}
	for _, path := range []string{seedsBase + "/media_" + strings.Repeat("A", 64), seedsBase + "/" + objectID + "/x",
		seedsBase + "/", seedsBase + "/x"} {
		request(t, h, http.MethodDelete, path, "", testToken, 400)
	}
	request(t, h, http.MethodDelete, seedsBase+"/"+objectID, "{}", testToken, 400) // DELETE has no body
	request(t, h, http.MethodDelete, seedsBase+"/"+objectID+"?force=1", "", testToken, 400)
	request(t, h, http.MethodGet, seedsBase+"?all=1", "", testToken, 400)
	request(t, h, http.MethodGet, seedsBase+"/"+objectID, "", testToken, 405)
	request(t, h, http.MethodPost, seedsBase+"/"+objectID, "{}", testToken, 405)
	request(t, h, http.MethodPut, seedsBase, seedBody(nil), testToken, 405)
	request(t, h, http.MethodGet, seedsBase+"x", "", testToken, 404)
}

func TestSeedRoutesAreUnavailableOnTheMockEngineAndOnWorkerFailure(t *testing.T) {
	mock := newHandler(testToken, engine.New(time.Minute, nil))
	request(t, mock, http.MethodGet, seedsBase, "", testToken, 404)
	request(t, mock, http.MethodPost, seedsBase, seedBody(nil), testToken, 404)
	failing := newSeedEngine()
	failing.failAll = true
	h := newHandler(testToken, failing)
	for _, got := range []map[string]any{
		request(t, h, http.MethodPost, seedsBase, seedBody(nil), testToken, 503),
		request(t, h, http.MethodGet, seedsBase, "", testToken, 503),
	} {
		if len(got) != 1 || got["error"] != "unavailable" {
			t.Fatalf("worker errors must not be echoed: %#v", got)
		}
	}
}

func TestSeedTrafficNeverLogsURLsTokensOrHashes(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	e := newSeedEngine()
	server := newServer(newHandler(testToken, e), logger)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, server, listener, time.Second, logger) }()
	base := "http://" + listener.Addr().String()
	send := func(method, path, body, token string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	send(http.MethodPost, seedsBase, seedBody(nil), testToken)
	send(http.MethodPost, seedsBase, seedBody(map[string]any{"tracker_url": "https://evil.test/announce/YWJj.ZGVm"}), testToken)
	send(http.MethodPost, seedsBase, seedBody(map[string]any{"extra": torrentURL}), testToken)
	send(http.MethodGet, seedsBase, "", "wrong-"+testToken)
	send(http.MethodDelete, seedsBase+"/"+objectID, "", testToken)
	e.failAll = true
	send(http.MethodPost, seedsBase, seedBody(nil), testToken)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	text := logs.String()
	for _, secret := range []string{testToken, trackerURL, torrentURL, "YWJj.ZGVm", seedHash, "evil.test", "Bearer"} {
		if strings.Contains(text, secret) {
			t.Fatalf("log contains %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "server_started") {
		t.Fatalf("expected server logs to be captured: %s", text)
	}
}
