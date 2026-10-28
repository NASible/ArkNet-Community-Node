package backend

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"arknet/transfer/engine"
)

const seedTracker = "https://tracker.arknet.test/announce/YWJj.ZGVm"
const seedTorrent = "https://core.arknet.test/api/torrent/v1/media/" + testID + "/YWJj.ZGVm"

// seedWorker records every protocol line and answers seed operations. When
// mode is "bad", it returns malformed seed records.
func seedWorker(t *testing.T, mode string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path, record := filepath.Join(dir, "worker.py"), filepath.Join(dir, "requests.jsonl")
	script := `#!/usr/bin/env python3
import json, sys
print(json.dumps({"ready": True, "backend": "libtorrent", "version": "2.0.10.0", "policy": {"v2_only": True, "dht": False, "pex": False, "lsd": False}}), flush=True)
MODE, seeds = ` + "\"" + mode + "\"" + `, {}
for line in sys.stdin:
    request = json.loads(line)
    with open(` + "\"" + record + "\"" + `, "a") as out:
        out.write(line)
    op, result = request["op"], {}
    if op == "shutdown":
        print(json.dumps({"id": request["id"], "ok": True}), flush=True)
        break
    if op == "seed":
        seed = {"object_id": request["object_id"], "state": "CHECKING", "total_bytes": request["size"], "uploaded_bytes": 0}
        if MODE == "bad": seed["state"] = "DOWNLOADING"
        seeds[request["object_id"]] = seed
        result = {"seed": seed}
    elif op == "unseed":
        if request["object_id"] in seeds:
            seed = seeds.pop(request["object_id"])
            result = {"seed": dict(seed, state="REMOVED", uploaded_bytes=0)}
        else:
            result = {"error": "not_found"}
    elif op == "seeds":
        items = list(seeds.values())
        if MODE == "bad": items = items + items
        result = {"seeds": items}
    result["id"] = request["id"]
    print(json.dumps(result), flush=True)
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, record
}

func startSeedWorker(t *testing.T, mode string) (*Client, string) {
	t.Helper()
	path, record := seedWorker(t, mode)
	client, err := Start(context.Background(), testConfig(t, path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, record
}

func recorded(t *testing.T, record string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var value map[string]any
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatal(err)
		}
		out = append(out, value)
	}
	return out
}

func TestSeedProtocolDerivesFileNameAndExposesOnlyIDAndState(t *testing.T) {
	client, record := startSeedWorker(t, "")
	status, err := client.Seed(testID, 4096, testHash, seedTracker, seedTorrent)
	if err != nil || status != (engine.SeedStatus{ObjectID: testID, State: engine.SeedChecking}) {
		t.Fatalf("seed: %+v %v", status, err)
	}
	lines := recorded(t, record)
	if len(lines) != 1 {
		t.Fatalf("requests %v", lines)
	}
	want := map[string]any{"op": "seed", "object_id": testID, "size": float64(4096), "info_hash_v2": testHash,
		"tracker_url": seedTracker, "torrent_url": seedTorrent, "file_name": testID + ".mp4"}
	for key, value := range want {
		if lines[0][key] != value {
			t.Fatalf("%s = %#v, want %#v", key, lines[0][key], value)
		}
	}
	if len(lines[0]) != len(want)+1 { // plus "id"
		t.Fatalf("unexpected protocol fields: %v", lines[0])
	}
	list, err := client.Seeds()
	if err != nil || len(list) != 1 || list[0] != status {
		t.Fatalf("seeds: %+v %v", list, err)
	}
	removed, err := client.Unseed(testID)
	if err != nil || removed.State != engine.SeedRemoved {
		t.Fatalf("unseed: %+v %v", removed, err)
	}
	if _, err := client.Unseed(testID); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("second unseed: %v", err)
	}
	if list, err := client.Seeds(); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("empty seeds must be a non-nil empty list: %#v %v", list, err)
	}
}

func TestSeedRejectsInvalidIDsAndUnauthorizedURLsBeforeTheWorker(t *testing.T) {
	client, record := startSeedWorker(t, "")
	other := "media_" + strings.Repeat("c", 64)
	cases := []struct {
		id, hash, tracker, torrent string
		size                       int64
	}{
		{"media_" + strings.Repeat("A", 64), testHash, seedTracker, seedTorrent, 1},
		{"../" + testID, testHash, seedTracker, seedTorrent, 1},
		{testID, strings.Repeat("B", 64), seedTracker, seedTorrent, 1},
		{testID, testHash, seedTracker, seedTorrent, -1},
		{testID, testHash, "https://evil.test/announce/YWJj.ZGVm", seedTorrent, 1},
		{testID, testHash, seedTracker + "?secret=1", seedTorrent, 1},
		{testID, testHash, "http://tracker.arknet.test/announce/YWJj.ZGVm", seedTorrent, 1},
		{testID, testHash, seedTracker, strings.Replace(seedTorrent, testID, other, 1), 1},
		{testID, testHash, seedTracker, "https://core.arknet.test/api/web-seed/v1/media/" + testID + "/YWJj.ZGVm", 1},
		{testID, testHash, seedTracker, "https://user:pw@core.arknet.test/api/torrent/v1/media/" + testID + "/YWJj.ZGVm", 1},
	}
	for _, c := range cases {
		_, err := client.Seed(c.id, c.size, c.hash, c.tracker, c.torrent)
		if !errors.Is(err, engine.ErrInvalid) || strings.Contains(err.Error(), "YWJj") || strings.Contains(err.Error(), "evil") {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if _, err := client.Unseed("media_bad"); !errors.Is(err, engine.ErrInvalid) {
		t.Fatalf("unseed invalid id: %v", err)
	}
	if lines := recorded(t, record); len(lines) != 0 {
		t.Fatalf("invalid requests reached the worker: %v", lines)
	}
}

func TestSeedRejectsMalformedWorkerResponses(t *testing.T) {
	client, _ := startSeedWorker(t, "bad")
	if _, err := client.Seed(testID, 4096, testHash, seedTracker, seedTorrent); err == nil {
		t.Fatal("accepted a seed in a transfer state")
	}
	if _, err := client.Seeds(); err == nil {
		t.Fatal("accepted duplicate seed records")
	}
}
