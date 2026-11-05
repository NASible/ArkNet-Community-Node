package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"arknet/transfer/engine"
)

type transferEngine interface {
	Add(string, int64, string) (engine.Status, error)
	Get(string) (engine.Status, error)
	List() []engine.Status
	Action(string, string) (engine.Status, error)
}

type authorizedTransferEngine interface {
	AddWithAuthorization(string, int64, string, string, string, string) (engine.Status, error)
}

func result(w http.ResponseWriter, status engine.Status, err error) {
	if err == nil {
		writeJSON(w, 200, status)
		return
	}
	switch {
	case errors.Is(err, engine.ErrNotFound):
		failure(w, 404, "not_found")
	case errors.Is(err, engine.ErrConflict):
		failure(w, 409, "conflict")
	case errors.Is(err, engine.ErrTransition):
		failure(w, 409, "invalid_transition")
	default:
		failure(w, 400, "invalid_request")
	}
}

func transferRoute(w http.ResponseWriter, r *http.Request, e transferEngine) {
	const prefix = "/api/transfer/v1/transfers"
	suffix := strings.TrimPrefix(r.URL.Path, prefix)
	if suffix == "" {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, 200, map[string]any{"transfers": e.List()})
		case http.MethodPost:
			fields, err := exactObjectWithOptionalFields(r, []string{"tracker_url", "web_seed_url", "torrent_url", "relays", "relay_list_expires_at"}, "object_id", "size", "info_hash_v2")
			var id, hash, trackerURL, webSeedURL, torrentURL string
			var size int64
			if err != nil || json.Unmarshal(fields["object_id"], &id) != nil ||
				json.Unmarshal(fields["size"], &size) != nil ||
				json.Unmarshal(fields["info_hash_v2"], &hash) != nil ||
				!validID(id) || size < 0 || !lowerHex(hash) {
				failure(w, 400, "invalid_request")
				return
			}
			relays, expiresAt, relayErr := parseRelays(fields)
			if relayErr != nil {
				failure(w, 400, "invalid_request")
				return
			}
			trackerRaw, hasTracker := fields["tracker_url"]
			webSeedRaw, hasWebSeed := fields["web_seed_url"]
			torrentRaw, hasTorrent := fields["torrent_url"]
			if hasTracker || hasWebSeed || hasTorrent {
				if !hasTracker || !hasWebSeed || !hasTorrent || json.Unmarshal(trackerRaw, &trackerURL) != nil ||
					json.Unmarshal(webSeedRaw, &webSeedURL) != nil || json.Unmarshal(torrentRaw, &torrentURL) != nil ||
					trackerURL == "" || webSeedURL == "" || torrentURL == "" {
					failure(w, 400, "invalid_request")
					return
				}
				if relayEngine, supported := e.(relayTransferEngine); supported {
					status, err := relayEngine.AddWithAuthorizationAndRelays(id, size, hash, trackerURL, webSeedURL, torrentURL, relays, expiresAt)
					result(w, status, err)
					return
				}
				authorizedEngine, supported := e.(authorizedTransferEngine)
				if !supported || len(relays) != 0 {
					failure(w, 400, "invalid_request")
					return
				}
				status, err := authorizedEngine.AddWithAuthorization(id, size, hash, trackerURL, webSeedURL, torrentURL)
				result(w, status, err)
				return
			}
			if len(relays) != 0 {
				failure(w, 400, "invalid_request")
				return
			}
			status, err := e.Add(id, size, hash)
			result(w, status, err)
		default:
			failure(w, 405, "method_not_allowed")
		}
		return
	}
	if !strings.HasPrefix(suffix, "/") {
		failure(w, 400, "invalid_request")
		return
	}
	parts := strings.Split(strings.TrimPrefix(suffix, "/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !validID(parts[0]) || (len(parts) == 2 && parts[1] == "") {
		failure(w, 400, "invalid_request")
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			failure(w, 405, "method_not_allowed")
			return
		}
		status, err := e.Get(parts[0])
		result(w, status, err)
		return
	}
	action := parts[1]
	if action != "pause" && action != "resume" && action != "cancel" {
		failure(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		failure(w, 405, "method_not_allowed")
		return
	}
	if _, err := exactObject(r); err != nil {
		failure(w, 400, "invalid_request")
		return
	}
	status, err := e.Action(parts[0], action)
	result(w, status, err)
}

func lowerHex(s string) bool { return engine.LowerHex(s) }
func validID(s string) bool  { return engine.ValidObjectID(s) }
