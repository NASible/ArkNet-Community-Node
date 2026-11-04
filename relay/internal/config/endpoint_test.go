package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

func TestEndpointKeyValidation(t *testing.T) {
	for _, name := range []string{"missing file", "seed only", "unpadded", "bad base64", "inconsistent", "grant reuse", "revocation reuse", "embedded newline", "noncanonical padding"} {
		t.Run(name, func(t *testing.T) {
			env := baseEnv(t)
			path := env["ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE"]
			_, key, _ := ed25519.GenerateKey(nil)
			encoded := base64.StdEncoding.EncodeToString(key)
			switch name {
			case "missing file":
				env["ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE"] = path + ".missing"
			case "seed only":
				encoded = base64.StdEncoding.EncodeToString(key.Seed())
			case "unpadded":
				encoded = strings.TrimRight(encoded, "=")
			case "bad base64":
				encoded = "!"
			case "inconsistent":
				key[63] ^= 1
				encoded = base64.StdEncoding.EncodeToString(key)
			case "grant reuse":
				_ = os.WriteFile(env["ARKNET_RELAY_GRANT_VERIFY_KEY_FILE"], []byte(base64.StdEncoding.EncodeToString(key[32:])), 0600)
			case "revocation reuse":
				_ = os.WriteFile(env["ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE"], []byte(base64.StdEncoding.EncodeToString(key[32:])), 0600)
			case "embedded newline":
				encoded = encoded[:20] + "\n" + encoded[20:]
			case "noncanonical padding":
				encoded = encoded[:85] + "B=="
			}
			if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := load(env); err == nil {
				t.Fatal("invalid endpoint key accepted")
			}
		})
	}
}

func TestEndpointKeyLoaded(t *testing.T) {
	env := baseEnv(t)
	data, _ := os.ReadFile(env["ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE"])
	expected, _ := base64.StdEncoding.DecodeString(string(data))
	if err := os.WriteFile(env["ARKNET_RELAY_ENDPOINT_SIGNING_KEY_FILE"], append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EndpointSigningKey.Equal(ed25519.PrivateKey(expected)) {
		t.Fatal("wrong endpoint key")
	}
}
