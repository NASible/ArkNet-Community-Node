package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"arknet/core/internal/assertions"
)

const maxPublishedAssertionKeys = 8

// loadNodeLoginConfig enables Core-bound Node login only when an assertion
// signing key is configured. The key is dedicated: it must differ from the
// tracker signing key. The published-keys file lists extra Ed25519 public keys
// (base64, one per line; '#' comments allowed) that Core publishes but never
// signs with: the next key before a rotation and retired keys afterwards.
func loadNodeLoginConfig(getenv func(string) string, tracker *trackerGrantIssuer) (*nodeLoginConfig, error) {
	keyFile := getenv("ARKNET_ASSERTION_SIGNING_KEY_FILE")
	publishedFile := getenv("ARKNET_ASSERTION_PUBLISHED_KEYS_FILE")
	window := getenv("ARKNET_ASSERTION_OFFLINE_WINDOW")
	if keyFile == "" {
		if publishedFile != "" || window != "" {
			return nil, fmt.Errorf("ARKNET_ASSERTION_PUBLISHED_KEYS_FILE and ARKNET_ASSERTION_OFFLINE_WINDOW require ARKNET_ASSERTION_SIGNING_KEY_FILE")
		}
		return nil, nil
	}
	key, err := readEd25519PrivateKey("ARKNET_ASSERTION_SIGNING_KEY_FILE", keyFile)
	if err != nil {
		return nil, err
	}
	if tracker != nil && subtle.ConstantTimeCompare(tracker.privateKey, key) == 1 {
		return nil, fmt.Errorf("ARKNET_ASSERTION_SIGNING_KEY_FILE must be a dedicated key, not the tracker signing key")
	}
	offline := assertions.MaxOfflineWindow
	if window != "" {
		offline, err = time.ParseDuration(window)
		if err != nil || offline < assertions.MinOfflineWindow || offline > assertions.MaxOfflineWindow {
			return nil, fmt.Errorf("ARKNET_ASSERTION_OFFLINE_WINDOW must be a Go duration between 1m and 720h (30 days)")
		}
	}
	signer, err := assertions.NewSigner(key, offline)
	if err != nil {
		return nil, err
	}
	var published []ed25519.PublicKey
	if publishedFile != "" {
		var forbidden []ed25519.PublicKey
		if tracker != nil {
			forbidden = append(forbidden, tracker.privateKey.Public().(ed25519.PublicKey))
		}
		published, err = readPublishedAssertionKeys(publishedFile, signer.PublicKey(), forbidden...)
		if err != nil {
			return nil, err
		}
	}
	return &nodeLoginConfig{signer: signer, published: published}, nil
}

// readEd25519PrivateKey reads one base64-encoded 64-byte Ed25519 private key,
// the same format as ARKNET_TRACKER_SIGNING_KEY_FILE.
func readEd25519PrivateKey(name, path string) (ed25519.PrivateKey, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", name, err)
	}
	defer file.Close()
	const encodedPrivateKeySize = 88
	encoded, err := io.ReadAll(io.LimitReader(file, encodedPrivateKeySize+2))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	invalid := fmt.Errorf("%s must contain a base64-encoded 64-byte Ed25519 private key", name)
	raw, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSuffix(string(encoded), "\n"))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, invalid
	}
	if subtle.ConstantTimeCompare(raw, ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])) != 1 {
		return nil, fmt.Errorf("%s contains an inconsistent Ed25519 private key", name)
	}
	return ed25519.PrivateKey(raw), nil
}

func readPublishedAssertionKeys(path string, active ed25519.PublicKey, forbidden ...ed25519.PublicKey) ([]ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ARKNET_ASSERTION_PUBLISHED_KEYS_FILE: %w", err)
	}
	var keys []ed25519.PublicKey
	seen := map[assertions.KeyID]bool{assertions.KeyIDFor(active): true}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		raw, err := base64.StdEncoding.Strict().DecodeString(text)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("ARKNET_ASSERTION_PUBLISHED_KEYS_FILE line %d must be a base64-encoded 32-byte Ed25519 public key", line)
		}
		for _, key := range forbidden {
			if key.Equal(ed25519.PublicKey(raw)) {
				return nil, fmt.Errorf("ARKNET_ASSERTION_PUBLISHED_KEYS_FILE line %d is the tracker public key; assertion keys must be dedicated", line)
			}
		}
		id := assertions.KeyIDFor(raw)
		if seen[id] {
			return nil, fmt.Errorf("ARKNET_ASSERTION_PUBLISHED_KEYS_FILE line %d repeats a key (or the active key)", line)
		}
		seen[id] = true
		keys = append(keys, ed25519.PublicKey(raw))
		if len(keys) > maxPublishedAssertionKeys {
			return nil, fmt.Errorf("ARKNET_ASSERTION_PUBLISHED_KEYS_FILE lists more than %d keys", maxPublishedAssertionKeys)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read ARKNET_ASSERTION_PUBLISHED_KEYS_FILE: %w", err)
	}
	return keys, nil
}
