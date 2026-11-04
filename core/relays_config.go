package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
)

// Operator relays (Prompt 48B, docs/private-relay-design.md). Core lists them
// to Nodes in a signed relay list and pushes revocations to each one.
const (
	maxConfiguredRelays = 16
	maxRelaysFileBytes  = 64 << 10
)

var (
	relayIDPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	spkiPinPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	relayHostLabels = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
)

type relayEntry struct {
	ID         string
	Address    string // host:port Nodes dial (TLS 1.3)
	SPKISHA256 string // hex SHA-256 of the relay certificate's SubjectPublicKeyInfo
	ControlURL *url.URL
	// revocationKey signs revocations for this relay only; the relay holds
	// the matching public key (ARKNET_RELAY_REVOCATION_VERIFY_KEY_FILE).
	revocationKey ed25519.PrivateKey
}

type relaysConfig struct {
	entries []relayEntry
	listKey ed25519.PrivateKey // signs the relay list Nodes fetch
}

// loadRelaysConfig reads ARKNET_RELAYS_FILE and ARKNET_RELAY_LIST_SIGNING_KEY_FILE.
// Every key must be distinct from every other relay key and from the
// tracker and assertion keys (otherKeys).
func loadRelaysConfig(getenv func(string) string, tracker *trackerGrantIssuer, otherKeys ...ed25519.PrivateKey) (*relaysConfig, error) {
	file, listKeyFile := getenv("ARKNET_RELAYS_FILE"), getenv("ARKNET_RELAY_LIST_SIGNING_KEY_FILE")
	if file == "" && listKeyFile == "" {
		return nil, nil
	}
	if file == "" || listKeyFile == "" {
		return nil, fmt.Errorf("ARKNET_RELAYS_FILE and ARKNET_RELAY_LIST_SIGNING_KEY_FILE must be configured together")
	}
	if tracker == nil {
		// Revocations name tracker-grant subjects, which only the tracker key derives.
		return nil, fmt.Errorf("relays require the private tracker (ARKNET_TRACKER_SIGNING_KEY_FILE)")
	}
	listKey, err := readEd25519PrivateKey("ARKNET_RELAY_LIST_SIGNING_KEY_FILE", listKeyFile)
	if err != nil {
		return nil, err
	}
	raw, err := readLimited(file, maxRelaysFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read ARKNET_RELAYS_FILE: %w", err)
	}
	var listed []struct {
		ID                string `json:"id"`
		Address           string `json:"address"`
		SPKISHA256        string `json:"spki_sha256"`
		ControlURL        string `json:"control_url"`
		RevocationKeyFile string `json:"revocation_key_file"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&listed); err != nil {
		return nil, fmt.Errorf("ARKNET_RELAYS_FILE must be a JSON array of relays: %w", err)
	}
	if len(listed) == 0 || len(listed) > maxConfiguredRelays {
		return nil, fmt.Errorf("ARKNET_RELAYS_FILE must list 1 to %d relays", maxConfiguredRelays)
	}
	cfg := &relaysConfig{listKey: listKey}
	keys := []ed25519.PrivateKey{tracker.privateKey, listKey}
	keys = append(keys, otherKeys...)
	seen := map[string]bool{}
	for i, item := range listed {
		name := fmt.Sprintf("ARKNET_RELAYS_FILE relay %d", i+1)
		if !relayIDPattern.MatchString(item.ID) || seen[item.ID] {
			return nil, fmt.Errorf("%s: id must be unique, 1-64 of a-z 0-9 . -", name)
		}
		seen[item.ID] = true
		if !validRelayAddress(item.Address) {
			return nil, fmt.Errorf("%s: address must be host:port", name)
		}
		if !spkiPinPattern.MatchString(item.SPKISHA256) {
			return nil, fmt.Errorf("%s: spki_sha256 must be 64 lowercase hex characters", name)
		}
		control, err := url.Parse(item.ControlURL)
		if err != nil || (control.Scheme != "http" && control.Scheme != "https") || control.Host == "" || control.User != nil ||
			(control.Path != "" && control.Path != "/") || control.RawQuery != "" || control.ForceQuery || control.Fragment != "" {
			return nil, fmt.Errorf("%s: control_url must be an http(s) origin", name)
		}
		control.Path = ""
		key, err := readEd25519PrivateKey(name+" revocation_key_file", item.RevocationKeyFile)
		if err != nil {
			return nil, err
		}
		cfg.entries = append(cfg.entries, relayEntry{ID: item.ID, Address: item.Address, SPKISHA256: item.SPKISHA256,
			ControlURL: control, revocationKey: key})
		keys = append(keys, key)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] != nil && keys[j] != nil && bytes.Equal(keys[i].Public().(ed25519.PublicKey), keys[j].Public().(ed25519.PublicKey)) {
				return nil, fmt.Errorf("relay list and relay revocation keys must be distinct from each other and from the tracker and assertion keys")
			}
		}
	}
	return cfg, nil
}

func validRelayAddress(address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" || len(host) > 253 {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return false
	}
	return net.ParseIP(host) != nil || relayHostLabels.MatchString(host)
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file larger than %d bytes", limit)
	}
	return data, nil
}
