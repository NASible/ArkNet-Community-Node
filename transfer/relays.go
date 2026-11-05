package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"arknet/transfer/engine"
)

type relayTransferEngine interface {
	AddWithAuthorizationAndRelays(string, int64, string, string, string, string, []engine.Relay, int64) (engine.Status, error)
}
type relaySeedEngine interface {
	SeedWithRelays(string, int64, string, string, string, []engine.Relay, int64) (engine.SeedStatus, error)
}

// parseRelays uses exact-object parsing for every entry too: encoding/json's
// struct decoder alone silently accepts duplicate and case-aliased keys.
func parseRelays(fields map[string]json.RawMessage) ([]engine.Relay, int64, error) {
	var expiresAt int64
	if raw, ok := fields["relay_list_expires_at"]; ok && json.Unmarshal(raw, &expiresAt) != nil {
		return nil, 0, errBody
	}
	var entries []json.RawMessage
	if raw, ok := fields["relays"]; ok && json.Unmarshal(raw, &entries) != nil {
		return nil, 0, errBody
	}
	if len(entries) > 16 {
		return nil, 0, errBody
	}
	var relays []engine.Relay
	for _, raw := range entries {
		entry, err := exactObject(&http.Request{Body: io.NopCloser(bytes.NewReader(raw))}, "id", "address", "spki_sha256")
		var relay engine.Relay
		if err != nil || json.Unmarshal(entry["id"], &relay.ID) != nil || json.Unmarshal(entry["address"], &relay.Address) != nil || json.Unmarshal(entry["spki_sha256"], &relay.SPKISHA256) != nil {
			return nil, 0, errBody
		}
		relays = append(relays, relay)
	}
	if !engine.ValidRelays(relays, expiresAt, time.Now()) {
		return nil, 0, errBody
	}
	return relays, expiresAt, nil
}
