package backend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arknet/transfer/engine"
)

const testID = "media_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func fakeWorker(t *testing.T, ready string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.py")
	script := `#!/usr/bin/env python3
import json, os, sys
print(` + ready + `, flush=True)
statuses = {}
for line in sys.stdin:
    request = json.loads(line)
    op = request["op"]
    if op == "shutdown":
        print(json.dumps({"id": request["id"], "ok": True}), flush=True)
        break
    if op == "add":
        status = {"object_id": request["object_id"], "state": "DOWNLOADING", "total_bytes": request["size"], "completed_bytes": 0, "mock": False, "error_code": ""}
        statuses[request["object_id"]] = status
        result = {"status": status}
    elif op == "list": result = {"transfers": list(statuses.values())}
    elif op == "get": result = {"status": statuses.get(request["object_id"]), "error": "not_found" if request["object_id"] not in statuses else ""}
    else:
        status = statuses.get(request["object_id"])
        if status is None: result = {"error": "not_found"}
        else:
            states = {"pause": "PAUSED", "resume": "DOWNLOADING", "cancel": "CANCELLED"}
            status["state"] = states[op]
            result = {"status": status}
    result["id"] = request["id"]
    if not result.get("error"): result.pop("error", None)
    print(json.dumps(result), flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func testConfig(t *testing.T, executable string) Config {
	t.Helper()
	return Config{Executable: executable, TrackerOrigin: "https://tracker.arknet.test", WebSeedOrigin: "https://core.arknet.test", StagingDir: filepath.Join(t.TempDir(), "staging"), StateDir: filepath.Join(t.TempDir(), "state"), ListenInterfaces: "0.0.0.0:0", DownloadRateLimit: 1024}
}

func TestWorkerClientLifecycle(t *testing.T) {
	ready := `json.dumps({"ready": True, "backend": "libtorrent", "version": "2.0.10.0", "policy": {"v2_only": True, "dht": False, "pex": False, "lsd": False}})`
	client, err := Start(context.Background(), testConfig(t, fakeWorker(t, ready)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	status, err := client.AddWithAuthorization(testID, 4096, testHash,
		"https://tracker.arknet.test/announce/YWJj.ZGVm",
		"https://core.arknet.test/api/web-seed/v1/media/"+testID+"/YWJj.ZGVm",
		"https://core.arknet.test/api/torrent/v1/media/"+testID+"/YWJj.ZGVm")
	if err != nil || status.ObjectID != testID || status.TotalBytes != 4096 || status.Mock || status.State != engine.Downloading {
		t.Fatalf("add: %+v %v", status, err)
	}
	items := client.List()
	if len(items) != 1 || items[0] != status {
		t.Fatalf("list: %+v", items)
	}
	if status, err = client.Action(testID, "pause"); err != nil || status.State != engine.Paused {
		t.Fatalf("pause: %+v %v", status, err)
	}
	if status, err = client.Action(testID, "resume"); err != nil || status.State != engine.Downloading {
		t.Fatalf("resume: %+v %v", status, err)
	}
	if status, err = client.Action(testID, "cancel"); err != nil || status.State != engine.Cancelled {
		t.Fatalf("cancel: %+v %v", status, err)
	}
	if _, err = client.Get("media_" + strings.Repeat("c", 64)); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAddProtocolIncludesValidatedPerTransferAuthorization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.py")
	script := `#!/usr/bin/env python3
import json, os, sys
print(json.dumps({"ready":True,"backend":"libtorrent","version":"2","policy":{"v2_only":True,"dht":False,"pex":False,"lsd":False}}), flush=True)
for line in sys.stdin:
 r=json.loads(line)
 if r["op"]=="shutdown": print(json.dumps({"id":r["id"],"ok":True}),flush=True); break
 expected_tracker=os.environ["EXPECTED_TRACKER"]
 expected_seed=os.environ["EXPECTED_WEB_SEED"]
 expected_torrent=os.environ["EXPECTED_TORRENT"]
 if set(r)!={"id","op","object_id","size","info_hash_v2","tracker_url","web_seed_url","torrent_url"} or r["tracker_url"]!=expected_tracker or r["web_seed_url"]!=expected_seed or r["torrent_url"]!=expected_torrent:
  print(json.dumps({"id":r["id"],"error":"invalid_request"}),flush=True); continue
 print(json.dumps({"id":r["id"],"status":{"object_id":r["object_id"],"state":"DOWNLOADING","total_bytes":r["size"],"completed_bytes":0,"mock":False,"error_code":""}}),flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	url := "https://core.arknet.test/api/web-seed/v1/media/" + testID + "/YWJj.ZGVm"
	torrent := "https://core.arknet.test/api/torrent/v1/media/" + testID + "/YWJj.ZGVm"
	tracker := "https://tracker.arknet.test/announce/YWJj.ZGVm"
	t.Setenv("EXPECTED_TRACKER", tracker)
	t.Setenv("EXPECTED_WEB_SEED", url)
	t.Setenv("EXPECTED_TORRENT", torrent)
	client, err := Start(context.Background(), testConfig(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.AddWithAuthorization(testID, 4096, testHash, tracker, url, torrent); err != nil {
		t.Fatal(err)
	}
	client.Close()
}

func TestAddWithAuthorizationRejectsUnauthorizedTracker(t *testing.T) {
	ready := `json.dumps({"ready": True, "backend": "libtorrent", "version": "2", "policy": {"v2_only": True, "dht": False, "pex": False, "lsd": False}})`
	client, err := Start(context.Background(), testConfig(t, fakeWorker(t, ready)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	seed := "https://core.arknet.test/api/web-seed/v1/media/" + testID + "/YWJj.ZGVm"
	torrent := "https://core.arknet.test/api/torrent/v1/media/" + testID + "/YWJj.ZGVm"
	for _, candidate := range []string{"https://evil.test/announce/YWJj.ZGVm", "https://tracker.arknet.test/announce", "https://tracker.arknet.test/announce/YWJj.ZGVm?x=1", "https://tracker.arknet.test/announce/YWJj=.ZGVm"} {
		if _, err = client.AddWithAuthorization(testID, 4096, testHash, candidate, seed, torrent); !errors.Is(err, engine.ErrInvalid) {
			t.Fatalf("accepted unauthorized tracker %q: %v", candidate, err)
		}
	}
}

func TestAddWithWebSeedRejectsEveryUnauthorizedURLWithoutSecretLeak(t *testing.T) {
	ready := `json.dumps({"ready": True, "backend": "libtorrent", "version": "2", "policy": {"v2_only": True, "dht": False, "pex": False, "lsd": False}})`
	client, err := Start(context.Background(), testConfig(t, fakeWorker(t, ready)))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	good := "https://core.arknet.test/api/web-seed/v1/media/" + testID + "/YWJj.ZGVm"
	bad := []string{
		"https://evil.test/api/web-seed/v1/media/" + testID + "/YWJj.ZGVm",
		"https://core.arknet.test/other/" + testID + "/YWJj.ZGVm",
		good + "?secret=x", good + "#secret", "https://user:secret@core.arknet.test/api/web-seed/v1/media/" + testID + "/YWJj.ZGVm",
		strings.Replace(good, "/api/", "/%61pi/", 1), strings.Replace(good, "YWJj.ZGVm", "YWJj=.ZGVm", 1),
		strings.Replace(good, testID, "media_"+strings.Repeat("c", 64), 1), strings.Repeat("x", 2049),
	}
	for _, candidate := range bad {
		_, err = client.AddWithAuthorization(testID, 4096, testHash, "https://tracker.arknet.test/announce/YWJj.ZGVm", candidate, "https://core.arknet.test/api/torrent/v1/media/"+testID+"/YWJj.ZGVm")
		if !errors.Is(err, engine.ErrInvalid) {
			t.Fatalf("accepted unauthorized URL: %v", err)
		}
		encoded, _ := json.Marshal(err.Error())
		if strings.Contains(string(encoded), candidate) || strings.Contains(err.Error(), "secret") {
			t.Fatal("web-seed credential leaked")
		}
	}
}

func TestWorkerClientRejectsUnsafeReadinessPolicy(t *testing.T) {
	for name, ready := range map[string]string{
		"dht": `json.dumps({"ready": True, "backend": "libtorrent", "version": "2", "policy": {"v2_only": True, "dht": True, "pex": False, "lsd": False}})`,
		"pex": `json.dumps({"ready": True, "backend": "libtorrent", "version": "2", "policy": {"v2_only": True, "dht": False, "pex": True, "lsd": False}})`,
		"lsd": `json.dumps({"ready": True, "backend": "libtorrent", "version": "2", "policy": {"v2_only": True, "dht": False, "pex": False, "lsd": True}})`,
		"v1":  `json.dumps({"ready": True, "backend": "libtorrent", "version": "2", "policy": {"v2_only": False, "dht": False, "pex": False, "lsd": False}})`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if client, err := Start(ctx, testConfig(t, fakeWorker(t, ready))); err == nil {
				client.Close()
				t.Fatal("accepted unsafe worker")
			}
		})
	}
}
