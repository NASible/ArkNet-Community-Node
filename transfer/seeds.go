package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"arknet/transfer/engine"
)

const seedsPrefix = "/api/transfer/v1/seeds"

// seedEngine is implemented only by the real libtorrent backend. The mock
// engine never serves peers, so its seed routes return 404.
type seedEngine interface {
	Seed(id string, size int64, hash, trackerURL, torrentURL string) (engine.SeedStatus, error)
	Unseed(id string) (engine.SeedStatus, error)
	Seeds() ([]engine.SeedStatus, error)
}

func seedResult(w http.ResponseWriter, status engine.SeedStatus, err error) {
	if err == nil {
		writeJSON(w, 200, status)
		return
	}
	switch {
	case errors.Is(err, engine.ErrNotFound):
		failure(w, 404, "not_found")
	case errors.Is(err, engine.ErrConflict):
		failure(w, 409, "conflict")
	case errors.Is(err, engine.ErrInvalid):
		failure(w, 400, "invalid_request")
	default:
		failure(w, 503, "unavailable")
	}
}

func seedRoute(w http.ResponseWriter, r *http.Request, e transferEngine) {
	seeds, supported := e.(seedEngine)
	if !supported {
		failure(w, 404, "not_found")
		return
	}
	suffix := strings.TrimPrefix(r.URL.Path, seedsPrefix)
	if suffix == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := seeds.Seeds()
			if err != nil {
				failure(w, 503, "unavailable")
				return
			}
			if list == nil {
				list = []engine.SeedStatus{}
			}
			writeJSON(w, 200, map[string]any{"seeds": list})
		case http.MethodPost:
			fields, err := exactObjectWithOptionalFields(r, []string{"relays", "relay_list_expires_at"}, "object_id", "size", "info_hash_v2", "tracker_url", "torrent_url")
			var id, hash, trackerURL, torrentURL string
			var size int64
			if err != nil || json.Unmarshal(fields["object_id"], &id) != nil ||
				json.Unmarshal(fields["size"], &size) != nil ||
				json.Unmarshal(fields["info_hash_v2"], &hash) != nil ||
				json.Unmarshal(fields["tracker_url"], &trackerURL) != nil ||
				json.Unmarshal(fields["torrent_url"], &torrentURL) != nil ||
				!validID(id) || size < 0 || !lowerHex(hash) || trackerURL == "" || torrentURL == "" {
				failure(w, 400, "invalid_request")
				return
			}
			relays, expiresAt, relayErr := parseRelays(fields)
			if relayErr != nil {
				failure(w, 400, "invalid_request")
				return
			}
			if relaySeeds, supported := e.(relaySeedEngine); supported {
				status, err := relaySeeds.SeedWithRelays(id, size, hash, trackerURL, torrentURL, relays, expiresAt)
				seedResult(w, status, err)
				return
			}
			if len(relays) != 0 {
				failure(w, 400, "invalid_request")
				return
			}
			status, err := seeds.Seed(id, size, hash, trackerURL, torrentURL)
			seedResult(w, status, err)
		default:
			failure(w, 405, "method_not_allowed")
		}
		return
	}
	id := strings.TrimPrefix(suffix, "/")
	if !strings.HasPrefix(suffix, "/") || !validID(id) {
		failure(w, 400, "invalid_request")
		return
	}
	if r.Method != http.MethodDelete {
		failure(w, 405, "method_not_allowed")
		return
	}
	if body, err := io.ReadAll(r.Body); err != nil || len(body) != 0 {
		failure(w, 400, "invalid_request")
		return
	}
	status, err := seeds.Unseed(id)
	seedResult(w, status, err)
}
