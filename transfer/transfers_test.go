package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"arknet/transfer/engine"
)

const objectID = "media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const base = "/api/transfer/v1/transfers"
const webSeedURL = "https://core.arknet.test/api/web-seed/v1/media/" + objectID + "/YWJj.ZGVm"
const torrentURL = "https://core.arknet.test/api/torrent/v1/media/" + objectID + "/YWJj.ZGVm"
const trackerURL = "https://tracker.arknet.test/announce/YWJj.ZGVm"

type authorizationRecordingEngine struct {
	*engine.Engine
	trackerURL string
	webSeedURL string
	torrentURL string
}

func (e *authorizationRecordingEngine) AddWithAuthorization(id string, size int64, hash, trackerURL, webSeedURL, torrentURL string) (engine.Status, error) {
	e.trackerURL, e.webSeedURL, e.torrentURL = trackerURL, webSeedURL, torrentURL
	return e.Add(id, size, hash)
}

func addBody(id string, size int64) string {
	return fmt.Sprintf(`{"object_id":%q,"size":%d,"info_hash_v2":%q}`, id, size, strings.Repeat("d", 64))
}

func TestHTTPTransferLifecycle(t *testing.T) {
	now := time.Unix(100, 0)
	h := newHandler(testToken, engine.New(10*time.Second, func() time.Time { return now }))
	check := func(method, path, body string, code int) map[string]any {
		return request(t, h, method, path, body, testToken, code)
	}
	if v := check("GET", base, "", 200); len(v["transfers"].([]any)) != 0 {
		t.Fatal(v)
	}
	v := check("POST", base, addBody(objectID, 100), 200)
	if v["object_id"] != objectID || v["state"] != "DOWNLOADING" || v["completed_bytes"] != float64(0) || v["total_bytes"] != float64(100) || v["error_code"] != "" || len(v) != 6 {
		t.Fatal(v)
	}
	now = now.Add(time.Second)
	v = check("GET", base+"/"+objectID, "", 200)
	if v["completed_bytes"] != float64(10) {
		t.Fatal(v)
	}
	for _, action := range []string{"pause", "pause", "resume", "resume", "cancel", "cancel"} {
		check("POST", base+"/"+objectID+"/"+action, "{}", 200)
	}
	if v = check("POST", base, addBody(objectID, 101), 409); v["error"] != "conflict" {
		t.Fatal(v)
	}
	check("POST", base, `{"object_id":"`+objectID+`","size":100,"info_hash_v2":""}`, 400)
	check("POST", "/api/transfer/v1/mock/transfers/"+objectID+"/complete", "{}", 404)
	missing := "media_" + strings.Repeat("c", 64)
	check("GET", base+"/"+missing, "", 404)
	check("POST", base+"/"+missing+"/pause", "{}", 404)
	for _, path := range []string{base, base + "/" + objectID, base + "/" + objectID + "/cancel"} {
		request(t, h, http.MethodPut, path, "{}", testToken, 405)
		request(t, h, "POST", path, "{}", "", 401)
	}
}

func TestHTTPAddAcceptsAuthorizationPairWithoutChangingLegacyMockAdd(t *testing.T) {
	recorder := &authorizationRecordingEngine{Engine: engine.New(time.Hour, nil)}
	h := newHandler(testToken, recorder)
	body := strings.TrimSuffix(addBody(objectID, 100), "}") + `,"tracker_url":"` + trackerURL + `","web_seed_url":"` + webSeedURL + `","torrent_url":"` + torrentURL + `"}`
	v := request(t, h, http.MethodPost, base, body, testToken, 200)
	if v["object_id"] != objectID || recorder.trackerURL != trackerURL || recorder.webSeedURL != webSeedURL || recorder.torrentURL != torrentURL {
		t.Fatalf("status=%v authorization was not passed exactly", v)
	}
	for _, partial := range []string{
		strings.TrimSuffix(addBody(objectID, 100), "}") + `,"tracker_url":"` + trackerURL + `"}`,
		strings.TrimSuffix(addBody(objectID, 100), "}") + `,"web_seed_url":"` + webSeedURL + `"}`,
		strings.TrimSuffix(addBody(objectID, 100), "}") + `,"torrent_url":"` + torrentURL + `"}`,
	} {
		if got := request(t, h, http.MethodPost, base, partial, testToken, 400); got["error"] != "invalid_request" {
			t.Fatal(got)
		}
	}

	legacy := engine.New(time.Hour, nil)
	h = newHandler(testToken, legacy)
	if v = request(t, h, http.MethodPost, base, addBody(objectID, 100), testToken, 200); v["object_id"] != objectID {
		t.Fatal(v)
	}
	if v = request(t, h, http.MethodPost, base, body, testToken, 400); v["error"] != "invalid_request" {
		t.Fatal(v)
	}
}
