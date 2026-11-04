package config

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKey(t *testing.T, dir, name string) string {
	t.Helper()
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(public)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"relay"}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600)
	return certFile, keyFile
}

func baseEnv(t *testing.T) map[string]string {
	dir := t.TempDir()
	cert, key := writeCert(t, dir)
	_, endpoint, _ := ed25519.GenerateKey(nil)
	endpointPath := filepath.Join(dir, "endpoint.key")
	if err := os.WriteFile(endpointPath, []byte(base64.StdEncoding.EncodeToString(endpoint)), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"ARKNET_RELAY_GRANT_VERIFY_KEY_FILE":      writeKey(t, dir, "grant.key"),
		"ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE": writeKey(t, dir, "revocation.key"),
		"ARKNET_RELAY_ID":                         "relay-dc-1",
		"ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE":  endpointPath,
		"ARKNET_RELAY_TLS_CERT_FILE":              cert,
		"ARKNET_RELAY_TLS_KEY_FILE":               key,
	}
}

func load(env map[string]string) (Config, error) {
	return Load(func(name string) string { return env[name] })
}

func TestDefaults(t *testing.T) {
	cfg, err := load(baseEnv(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BindAddr != "127.0.0.1:7443" || cfg.ControlBindAddr != "127.0.0.1:7080" || cfg.MaxConnections != 4096 ||
		cfg.MaxGrantLifetime != 5*time.Minute || cfg.ShutdownTimeout != 10*time.Second || cfg.RelayID != "relay-dc-1" ||
		len(cfg.GrantKey) != ed25519.PublicKeySize || len(cfg.RevocationKey) != ed25519.PublicKeySize ||
		len(cfg.Certificate.Certificate) != 1 {
		t.Fatalf("%+v", cfg)
	}
}

func TestRejectsUnsafeConfiguration(t *testing.T) {
	for name, change := range map[string]func(map[string]string){
		"no endpoint key":   func(e map[string]string) { delete(e, "ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE") },
		"no grant key":      func(e map[string]string) { delete(e, "ARKNET_RELAY_GRANT_VERIFY_KEY_FILE") },
		"no revocation key": func(e map[string]string) { delete(e, "ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE") },
		"same key for both": func(e map[string]string) {
			e["ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE"] = e["ARKNET_RELAY_GRANT_VERIFY_KEY_FILE"]
		},
		"no relay ID":      func(e map[string]string) { delete(e, "ARKNET_RELAY_ID") },
		"bad relay ID":     func(e map[string]string) { e["ARKNET_RELAY_ID"] = "Relay 1" },
		"long relay ID":    func(e map[string]string) { e["ARKNET_RELAY_ID"] = strings.Repeat("a", 65) },
		"no certificate":   func(e map[string]string) { delete(e, "ARKNET_RELAY_TLS_CERT_FILE") },
		"bad bind address": func(e map[string]string) { e["ARKNET_RELAY_BIND_ADDR"] = "nope" },
		"same addresses":   func(e map[string]string) { e["ARKNET_RELAY_BIND_ADDR"] = "127.0.0.1:7080" },
		"zero connections": func(e map[string]string) { e["ARKNET_RELAY_MAX_CONNECTIONS"] = "0" },
		"too many conns":   func(e map[string]string) { e["ARKNET_RELAY_MAX_CONNECTIONS"] = "100000" },
		"short lifetime":   func(e map[string]string) { e["ARKNET_RELAY_MAX_GRANT_LIFETIME"] = "30s" },
		"long lifetime":    func(e map[string]string) { e["ARKNET_RELAY_MAX_GRANT_LIFETIME"] = "2h" },
		"padded-less key": func(e map[string]string) {
			_ = os.WriteFile(e["ARKNET_RELAY_GRANT_VERIFY_KEY_FILE"], []byte("AAAA"), 0o600)
		},
	} {
		env := baseEnv(t)
		change(env)
		if _, err := load(env); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
