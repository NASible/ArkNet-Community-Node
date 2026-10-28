package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProcessServesHealthAndStopsOnSIGTERM(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "arknet-tracker")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "verify-key")
	if err := os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(publicKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		"ARKNET_TRACKER_VERIFY_KEY_FILE="+keyFile,
		"ARKNET_TRACKER_BIND_ADDR=127.0.0.1:0",
	)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()

	logs := make(chan map[string]any, 10)
	go scanJSONLogs(stderr, logs)
	var address string
	deadline := time.After(3 * time.Second)
	for address == "" {
		select {
		case record := <-logs:
			if record["msg"] == "tracker_started" {
				address, _ = record["addr"].(string)
			}
		case <-deadline:
			t.Fatal("startup log not observed")
		}
	}
	response, err := http.Get("http://" + address + "/health")
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Fatalf("health status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("process exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit")
	}
	foundShutdown := false
	for record := range logs {
		if record["msg"] == "shutdown_complete" {
			foundShutdown = true
		}
	}
	if !foundShutdown {
		t.Fatal("shutdown_complete log not observed")
	}
}

func scanJSONLogs(reader io.Reader, output chan<- map[string]any) {
	defer close(output)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			output <- record
		}
	}
}
