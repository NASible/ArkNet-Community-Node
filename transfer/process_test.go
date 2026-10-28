package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func childEnv(extra ...string) []string {
	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "ARKNET_TRANSFER_") {
			env = append(env, v)
		}
	}
	return append(env, extra...)
}

func fakeWorker(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.py")
	program := `#!/usr/bin/env python3
import json,sys
items={}
print(json.dumps({"ready":True,"backend":"libtorrent","version":"test","policy":{"v2_only":True,"dht":False,"pex":False,"lsd":False}}),flush=True)
for line in sys.stdin:
 r=json.loads(line); i=r["id"]; op=r["op"]
 if op=="shutdown": print(json.dumps({"id":i,"ok":True}),flush=True); break
 if op=="list": print(json.dumps({"id":i,"transfers":list(items.values())}),flush=True); continue
 oid=r["object_id"]
 if op=="add": items[oid]={"object_id":oid,"state":"DOWNLOADING","total_bytes":r["size"],"completed_bytes":0,"mock":False,"error_code":""}
 if oid not in items: print(json.dumps({"id":i,"error":"not_found"}),flush=True); continue
 if op=="pause": items[oid]["state"]="PAUSED"
 if op=="resume": items[oid]["state"]="DOWNLOADING"
 if op=="cancel": items[oid]["state"]="CANCELLED"
 print(json.dumps({"id":i,"status":items[oid]}),flush=True)
`
	if err := os.WriteFile(path, []byte(program), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompiledProcessSIGTERM(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "arknet-transfer")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = childEnv(
		"ARKNET_TRANSFER_TOKEN="+testToken,
		"ARKNET_TRANSFER_ADDR=127.0.0.1:0",
		"ARKNET_TRANSFER_TRACKER_ORIGIN=https://tracker.arknet.test",
		"ARKNET_TRANSFER_WEB_SEED_ORIGIN=https://core.arknet.test",
		"ARKNET_TRANSFER_STAGING_DIR="+filepath.Join(root, "staging"),
		"ARKNET_TRANSFER_STATE_DIR="+filepath.Join(root, "state"),
		"ARKNET_TRANSFER_WORKER="+fakeWorker(t),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	address := make(chan string, 1)
	logsDone := make(chan string, 1)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	go func() {
		var logs strings.Builder
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			logs.WriteString(line + "\n")
			var record map[string]any
			if json.Unmarshal([]byte(line), &record) == nil && record["msg"] == "server_started" {
				address <- record["addr"].(string)
			}
		}
		logsDone <- logs.String()
	}()
	var addr string
	select {
	case addr = <-address:
	case <-ctx.Done():
		t.Fatalf("no startup record: %s", stderr.String())
	}
	client := &http.Client{Timeout: 2 * time.Second}
	call := func(method, path, body string, auth bool, want int) map[string]any {
		t.Helper()
		r, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if auth {
			r.Header.Set("Authorization", "Bearer "+testToken)
		}
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v map[string]any
		if err = json.NewDecoder(resp.Body).Decode(&v); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != want || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatal(resp.StatusCode, v)
		}
		return v
	}
	call("GET", "/health", "", false, 200)
	call("GET", "/api/transfer/v1/version", "", false, 401)
	authorizedBody := strings.TrimSuffix(addBody(objectID, 100), "}") +
		`,"tracker_url":"` + trackerURL + `","web_seed_url":"` + webSeedURL + `","torrent_url":"` + torrentURL + `"}`
	v := call("POST", base, authorizedBody, true, 200)
	if v["mock"] != false || v["state"] != "DOWNLOADING" {
		t.Fatal(v)
	}
	call("POST", base+"/"+objectID+"/pause", "{}", true, 200)
	call("POST", base+"/"+objectID+"/resume", "{}", true, 200)
	call("POST", base+"/"+objectID+"/cancel", "{}", true, 200)
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	logs := <-logsDone
	if err = cmd.Wait(); err != nil {
		t.Fatalf("SIGTERM: %v %s", err, stderr.String())
	}
	if !strings.Contains(logs, `"msg":"shutdown_complete"`) || strings.Contains(logs, testToken) {
		t.Fatal("missing shutdown evidence or token leak")
	}
	t.Log("real listener served authenticated libtorrent-backend API; SIGTERM exit 0")

	invalid := exec.Command(binary)
	invalid.Env = childEnv("ARKNET_TRANSFER_TOKEN=" + testToken + "\n")
	out, err := invalid.CombinedOutput()
	if err == nil || !bytes.Contains(out, []byte(`"msg":"configuration_failed"`)) || bytes.Contains(out, []byte(testToken)) {
		t.Fatalf("invalid config result: %v %s", err, out)
	}
}
