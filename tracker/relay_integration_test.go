package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercises the deployed boundary: relay emits the token; the separately
// compiled tracker receives only public keys and advertises that endpoint.
func TestRealRelayRegistrationTokenIsAcceptedByTracker(t *testing.T) {
	dir := t.TempDir()
	build := func(module, name string) string {
		t.Helper()
		binary := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", binary, ".")
		cmd.Dir = module
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v %s", name, err, out)
		}
		return binary
	}
	relayBin := build("../relay", "relay")
	trackerBin := build(".", "tracker")
	keyfile := func(name string) (ed25519.PrivateKey, string, string) {
		t.Helper()
		pub, key, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		privPath, pubPath := filepath.Join(dir, name+".key"), filepath.Join(dir, name+".pub")
		if err := os.WriteFile(privPath, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pubPath, []byte(base64.StdEncoding.EncodeToString(pub)), 0600); err != nil {
			t.Fatal(err)
		}
		return key, privPath, pubPath
	}
	grantKey, _, grantPub := keyfile("grant")
	_, endpointPriv, endpointPub := keyfile("endpoint")
	_, _, revocationPub := keyfile("revocation")
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"relay"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &tlsKey.PublicKey, tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(tlsKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool.AddCert(parsed)
	start := func(binary, event string, env []string) map[string]any {
		t.Helper()
		cmd := exec.Command(binary)
		cmd.Env = env
		stderr, err := cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Signal(syscall.SIGTERM)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("process exit: %v", err)
				}
			case <-time.After(15 * time.Second):
				_ = cmd.Process.Kill()
				<-done
				t.Error("process shutdown timed out")
			}
		})
		records := make(chan map[string]any, 32)
		go func() {
			defer close(records)
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				var m map[string]any
				if json.Unmarshal(scanner.Bytes(), &m) == nil {
					select {
					case records <- m:
					default:
					}
				}
			}
		}()
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for {
			select {
			case m, ok := <-records:
				if !ok {
					t.Fatal("process exited before readiness")
				}
				if m["msg"] == event {
					return m
				}
			case <-timer.C:
				t.Fatal("process readiness timed out")
			}
		}
	}
	relay := start(relayBin, "relay_started", []string{
		"ARKNET_RELAY_ID=relay-integration", "ARKNET_RELAY_BIND_ADDR=127.0.0.1:0", "ARKNET_RELAY_CONTROL_BIND_ADDR=127.0.0.1:0",
		"ARKNET_RELAY_GRANT_VERIFY_KEY_FILE=" + grantPub, "ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE=" + revocationPub,
		"ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE=" + endpointPriv, "ARKNET_RELAY_TLS_CERT_FILE=" + certPath, "ARKNET_RELAY_TLS_KEY_FILE=" + keyPath,
	})
	tracker := start(trackerBin, "tracker_started", []string{"ARKNET_TRACKER_BIND_ADDR=127.0.0.1:0", "ARKNET_TRACKER_VERIFY_KEY_FILE=" + grantPub, "ARKNET_TRACKER_RELAY_VERIFY_KEYS_FILE=" + endpointPub})
	conn, err := tls.Dial("tcp", relay["addr"].(string), &tls.Config{RootCAs: pool, ServerName: "relay", MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hash := sha256.Sum256([]byte("integration swarm"))
	subject := [16]byte{1}
	grant := func(s [16]byte, actions byte) string {
		p := make([]byte, 58)
		p[0] = 2
		binary.BigEndian.PutUint64(p[1:9], uint64(time.Now().Add(time.Minute).Unix()))
		copy(p[9:25], s[:])
		copy(p[25:57], hash[:])
		p[57] = actions
		return base64.RawURLEncoding.EncodeToString(p) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(grantKey, p))
	}
	if err := json.NewEncoder(conn).Encode(map[string]string{"op": "listen", "grant": grant(subject, 2)}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var registration struct {
		Endpoint      string `json:"endpoint"`
		EndpointToken string `json:"endpoint_token"`
		Expires       int64  `json:"expires_at"`
	}
	if json.Unmarshal(line, &registration) != nil || registration.EndpointToken == "" {
		t.Fatal("relay returned no endpoint token")
	}
	request := func(s [16]byte, left, token string) (int, []byte) {
		t.Helper()
		v := url.Values{"info_hash": {string(hash[:20])}, "peer_id": {string(append(s[:], []byte("peer")...))}, "port": {"9999"}, "left": {left}, "compact": {"1"}, "uploaded": {"0"}, "downloaded": {"0"}}
		if token != "" {
			v.Set("endpoint_token", token)
		}
		action := byte(1)
		if left == "0" {
			action = 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "GET", "http://"+tracker["addr"].(string)+"/announce/"+grant(s, action)+"?"+v.Encode(), nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}
	if status, _ := request(subject, "0", registration.EndpointToken); status != 200 {
		t.Fatalf("relay token rejected: %d", status)
	}
	status, body := request([16]byte{2}, "1", "")
	if status != 200 {
		t.Fatal(status)
	}
	payload, _, _ := strings.Cut(registration.EndpointToken, ".")
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(raw) != 63 {
		t.Fatal("bad relay token payload")
	}
	want := "5:peers6:" + string(raw[57:63]) + "6:peers60:"
	if !strings.Contains(string(body), want) {
		t.Fatalf("tracker did not advertise relay's actual endpoint")
	}
	if status, _ := request([16]byte{3}, "0", registration.EndpointToken); status != 403 {
		t.Fatalf("another subject reused token: %d", status)
	}
	if status, _ := request([16]byte{4}, "0", ""); status != 403 {
		t.Fatalf("tokenless seed accepted: %d", status)
	}
}
