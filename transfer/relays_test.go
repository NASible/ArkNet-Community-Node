package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"arknet/transfer/engine"
)

type relayRecordingEngine struct {
	*seedRecordingEngine
	relays     []engine.Relay
	expires    int64
	relayCalls int
}

func (e *relayRecordingEngine) AddWithAuthorizationAndRelays(id string, size int64, hash, tracker, webseed, torrent string, relays []engine.Relay, expires int64) (engine.Status, error) {
	e.relays, e.expires = relays, expires
	e.relayCalls++
	return e.Add(id, size, hash)
}
func (e *relayRecordingEngine) SeedWithRelays(id string, size int64, hash, tracker, torrent string, relays []engine.Relay, expires int64) (engine.SeedStatus, error) {
	e.relays, e.expires = relays, expires
	e.relayCalls++
	return e.Seed(id, size, hash, tracker, torrent)
}
func relayBody(path, entries, expiry string) string {
	body := seedBody(nil)
	if path == base {
		body = strings.TrimSuffix(body, "}") + `,"web_seed_url":` + fmt.Sprintf("%q", webSeedURL) + `}`
	}
	return strings.TrimSuffix(body, "}") + `,"relays":` + entries + `,"relay_list_expires_at":` + expiry + `}`
}
func TestHTTPRelayAuthorizationAddRefreshAndSeed(t *testing.T) {
	relays := []engine.Relay{{ID: "relay-1", Address: "relay.example:443", SPKISHA256: strings.Repeat("a", 64)}}
	data, _ := json.Marshal(relays)
	expires := time.Now().Add(time.Hour).Unix()
	for _, path := range []string{base, seedsBase} {
		t.Run(path, func(t *testing.T) {
			e := &relayRecordingEngine{seedRecordingEngine: newSeedEngine()}
			h := newHandler(testToken, e)
			for _, entry := range []string{string(data), "[]"} {
				expiry := expires
				if entry == "[]" {
					expiry = 0
				}
				response := request(t, h, http.MethodPost, path, relayBody(path, entry, fmt.Sprint(expiry)), testToken, 200)
				if e.expires != expiry || (entry != "[]" && !reflect.DeepEqual(e.relays, relays)) || (entry == "[]" && len(e.relays) != 0) {
					t.Fatal("authorization not replaced")
				}
				for _, field := range []string{"relays", "relay_list_expires_at", "tracker_url", "torrent_url", "web_seed_url", "spki_sha256"} {
					if _, ok := response[field]; ok {
						t.Fatalf("response leaked %s", field)
					}
				}
			}
			if e.relayCalls != 2 {
				t.Fatalf("expected add/refresh calls, got %d", e.relayCalls)
			}
		})
	}
}
func TestHTTPRelayListAllowsSixteenBoundedLongEndpoints(t *testing.T) {
	var relays []engine.Relay
	for i := 0; i < 16; i++ {
		relays = append(relays, engine.Relay{ID: fmt.Sprintf("relay-%d", i), Address: fmt.Sprintf("r%d.%s.%s.%s.example:443", i, strings.Repeat("a", 63), strings.Repeat("b", 63), strings.Repeat("c", 63)), SPKISHA256: fmt.Sprintf("%064x", i)})
	}
	data, _ := json.Marshal(relays)
	for _, path := range []string{base, seedsBase} {
		e := &relayRecordingEngine{seedRecordingEngine: newSeedEngine()}
		body := relayBody(path, string(data), fmt.Sprint(time.Now().Add(time.Hour).Unix()))
		if len(body) <= 4096 {
			t.Fatal("test must exercise expanded request bound")
		}
		request(t, newHandler(testToken, e), http.MethodPost, path, body, testToken, 200)
		if !reflect.DeepEqual(e.relays, relays) {
			t.Fatal("relay list truncated")
		}
	}
}

func TestHTTPRelaySchemasRejectBeforeEngine(t *testing.T) {
	good := `{"id":"relay-1","address":"relay.example:443","spki_sha256":"` + strings.Repeat("a", 64) + `"}`
	for _, path := range []string{base, seedsBase} {
		cases := []struct{ entries, expiry string }{
			{"[" + good + "]", "0"}, {"[]", "123"}, {"null", "0"}, {"{}", "0"}, {"[]", `"0"`}, {"[]", "null"}, {"[]", "0.5"},
			{"[" + strings.TrimSuffix(good, "}") + `,"id":"relay-2"}]`, "4102444800"},
			{"[" + strings.TrimSuffix(good, "}") + `,"i\u0064":"relay-2"}]`, "4102444800"},
			{"[" + strings.Replace(good, `"id"`, `"ID"`, 1) + "]", "4102444800"},
			{"[" + strings.TrimSuffix(good, "}") + `,"unknown":1}]`, "4102444800"},
			{"[" + strings.Replace(good, `"id":"relay-1",`, "", 1) + "]", "4102444800"},
			{"[" + strings.Replace(good, `"relay-1"`, `null`, 1) + "]", "4102444800"},
		}
		for _, tc := range cases {
			e := &relayRecordingEngine{seedRecordingEngine: newSeedEngine()}
			request(t, newHandler(testToken, e), http.MethodPost, path, relayBody(path, tc.entries, tc.expiry), testToken, 400)
			if e.relayCalls != 0 || len(e.calls) != 0 || len(e.List()) != 0 {
				t.Fatal("invalid request reached engine")
			}
		}
	}
}
