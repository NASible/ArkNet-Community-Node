package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"arknet.local/relay/internal/revocation"
)

func writeKeyPair(t *testing.T, dir, name string) ed25519.PrivateKey {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(nil)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(base64.StdEncoding.EncodeToString(public)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return private
}

func writeCertificate(t *testing.T, dir string) *x509.CertPool {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay"}, DNSNames: []string{"relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	_ = os.WriteFile(filepath.Join(dir, "cert.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	certificate, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	return pool
}

func seedGrant(key ed25519.PrivateKey, subject [16]byte) string {
	payload := make([]byte, 58)
	payload[0] = 2
	binary.BigEndian.PutUint64(payload[1:9], uint64(time.Now().Add(2*time.Minute).Unix()))
	copy(payload[9:25], subject[:])
	for i := 25; i < 57; i++ {
		payload[i] = 0x42
	}
	payload[57] = 0x02
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
}

type logCapture struct {
	mu      sync.Mutex
	all     strings.Builder
	records chan map[string]any
}

func (l *logCapture) scan(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		l.mu.Lock()
		l.all.WriteString(scanner.Text() + "\n")
		l.mu.Unlock()
		var record map[string]any
		if json.Unmarshal(scanner.Bytes(), &record) == nil {
			select { // never block the relay's stderr
			case l.records <- record:
			default:
			}
		}
	}
	close(l.records)
}

func (l *logCapture) text() string { l.mu.Lock(); defer l.mu.Unlock(); return l.all.String() }

func TestProcessServesTLS13RevokesOnSignedRequestAndStopsOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "arknet-relay")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	grantKey := writeKeyPair(t, dir, "grant.pub")
	revocationKey := writeKeyPair(t, dir, "revocation.pub")
	_, endpointKey, _ := ed25519.GenerateKey(nil)
	if err := os.WriteFile(filepath.Join(dir, "endpoint.key"), []byte(base64.StdEncoding.EncodeToString(endpointKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	pool := writeCertificate(t, dir)
	command := exec.Command(binary)
	command.Env = append(os.Environ(),
		"ARKNET_RELAY_GRANT_VERIFY_KEY_FILE="+filepath.Join(dir, "grant.pub"),
		"ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE="+filepath.Join(dir, "revocation.pub"),
		"ARKNET_RELAY_ID=relay-test",
		"ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE="+filepath.Join(dir, "endpoint.key"),
		"ARKNET_RELAY_TLS_CERT_FILE="+filepath.Join(dir, "cert.pem"),
		"ARKNET_RELAY_TLS_KEY_FILE="+filepath.Join(dir, "key.pem"),
		"ARKNET_RELAY_BIND_ADDR=127.0.0.1:0",
		"ARKNET_RELAY_CONTROL_BIND_ADDR=127.0.0.1:0",
	)
	stderr, _ := command.StderrPipe()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	logs := &logCapture{records: make(chan map[string]any, 64)}
	go logs.scan(stderr)
	var relayAddr, controlAddr string
	for relayAddr == "" {
		select {
		case record, ok := <-logs.records:
			if !ok {
				t.Fatalf("relay exited at startup: %s", logs.text())
			}
			if record["msg"] == "relay_started" {
				relayAddr, _ = record["addr"].(string)
				controlAddr, _ = record["control_addr"].(string)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("relay did not start")
		}
	}
	controlBase := "http://" + controlAddr
	if resp, err := http.Get(controlBase + "/health"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("health: %v", err)
	}

	if _, err := tls.Dial("tcp", relayAddr, &tls.Config{RootCAs: pool, ServerName: "relay", MaxVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("TLS 1.2 handshake accepted")
	}
	conn, err := tls.Dial("tcp", relayAddr, &tls.Config{RootCAs: pool, ServerName: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	subject := [16]byte{0x5e, 0xed, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14}
	token := seedGrant(grantKey, subject)
	line, _ := json.Marshal(map[string]string{"op": "listen", "grant": token})
	_, _ = conn.Write(append(line, '\n'))
	reader := bufio.NewReader(conn)
	reply, err := reader.ReadBytes('\n')
	var registered map[string]any
	if err != nil || json.Unmarshal(reply, &registered) != nil || registered["endpoint"] == nil {
		t.Fatalf("registration: %q %v", reply, err)
	}

	endpointToken, _ := registered["endpoint_token"].(string)
	parts := strings.Split(endpointToken, ".")
	if len(parts) != 2 {
		t.Fatal("missing endpoint token")
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
	signature, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if len(payload) != 63 || !ed25519.Verify(endpointKey.Public().(ed25519.PublicKey), append([]byte("ARKNET-RELAY-ENDPOINT-V1\x00"), payload...), signature) {
		t.Fatal("invalid process endpoint signature")
	}
	grantParts := strings.Split(token, ".")
	grantPayload, _ := base64.RawURLEncoding.DecodeString(grantParts[0])
	if string(payload[1:9]) != string(grantPayload[1:9]) {
		t.Fatal("endpoint expiry differs from grant")
	}

	// A revocation signed with the grant key is refused; Core's revocation key works.
	wrongKey, _ := revocation.Sign(grantKey, "relay-test", [16]byte{1}, time.Now(), [][16]byte{subject})
	if resp, _ := http.Post(controlBase+"/api/relay/v1/revocations", "text/plain", strings.NewReader(wrongKey)); resp == nil || resp.StatusCode != 401 {
		t.Fatal("revocation signed with the grant key accepted")
	}
	signed, _ := revocation.Sign(revocationKey, "relay-test", [16]byte{2}, time.Now(), [][16]byte{subject})
	resp, err := http.Post(controlBase+"/api/relay/v1/revocations", "text/plain", strings.NewReader(signed))
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revocation: %v %v", resp, err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("revoked registration stayed open")
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("did not stop on SIGTERM")
	}
	text := logs.text()
	if !strings.Contains(text, "shutdown_complete") || !strings.Contains(text, "relay_revocation_received") {
		t.Fatalf("missing events: %s", text)
	}
	for _, value := range []string{token[:30], hex.EncodeToString(subject[:]), registered["endpoint"].(string),
		conn.LocalAddr().String(), signed[:30], endpointToken} {
		if strings.Contains(text, value) {
			t.Errorf("log contains %q", value)
		}
	}
}
