package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
)

// The optional file contains only operator-relay endpoint verification keys,
// one canonical padded-base64 public key per line. With no keys configured,
// seed publication is disabled; downloaders remain accepted and unadvertised.
func loadRelayKeys(path string, grantKey ed25519.PublicKey) ([]ed25519.PublicKey, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open relay verification keys: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, fmt.Errorf("relay verification keys file unreadable or oversized")
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 16 {
		return nil, fmt.Errorf("too many relay verification keys")
	}
	keys := make([]ed25519.PublicKey, 0, len(lines))
	seen := map[string]bool{}
	for _, line := range lines {
		text := strings.TrimSpace(line)
		key, err := base64.StdEncoding.Strict().DecodeString(text)
		if err != nil || len(key) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(key) != text || seen[text] || bytes.Equal(key, grantKey) {
			return nil, fmt.Errorf("invalid, duplicate or grant-reused relay verification key")
		}
		seen[text] = true
		keys = append(keys, ed25519.PublicKey(key))
	}
	return keys, nil
}
