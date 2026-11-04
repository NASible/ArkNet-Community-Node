package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/relays"
)

func TestRelayListKeyIsOptInAndValidated(t *testing.T) {
	cfg, err := loadConfig(envMap(map[string]string{"ARKNET_CORE_URL": "https://core.example"}))
	if err != nil || cfg.relayList != nil {
		t.Fatalf("relay list must be off without a key: %v %v", cfg.relayList, err)
	}
	public, _, _ := ed25519.GenerateKey(nil)
	cfg, err = loadConfig(envMap(map[string]string{"ARKNET_CORE_URL": "https://core.example",
		"ARKNET_RELAY_LIST_VERIFY_KEY": base64.StdEncoding.EncodeToString(public)}))
	if err != nil || cfg.relayList == nil || !cfg.relayList.key.Equal(public) || cfg.relayList.holder == nil {
		t.Fatalf("%+v %v", cfg.relayList, err)
	}
	for name, value := range map[string]string{
		"short":      base64.StdEncoding.EncodeToString(public[:31]),
		"unpadded":   strings.TrimRight(base64.StdEncoding.EncodeToString(public), "="),
		"url-safe":   strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(append([]byte{0xfb, 0xff}, public[2:]...))),
		"not base64": "not a key",
	} {
		if _, err := loadConfig(envMap(map[string]string{"ARKNET_CORE_URL": "https://core.example", "ARKNET_RELAY_LIST_VERIFY_KEY": value})); err == nil {
			t.Errorf("%s key accepted", name)
		}
	}
	if _, err := loadConfig(envMap(map[string]string{"ARKNET_RELAY_LIST_VERIFY_KEY": base64.StdEncoding.EncodeToString(public)})); err == nil {
		t.Error("relay list key without Core accepted")
	}
}

func TestHeartbeatRefreshFetchesAndKeepsTheVerifiedRelayList(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(nil)
	payload, _ := json.Marshal(map[string]any{"version": 1, "issued_at": time.Now().Unix(), "expires_at": time.Now().Add(time.Hour).Unix(),
		"relays": []map[string]string{{"id": "relay-dc-1", "address": "relay1.example.net:7443", "spki_sha256": strings.Repeat("a", 64)}}})
	token := base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, append([]byte(relays.Domain), payload...)))
	fetched := make(chan struct{}, 4)
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/relays") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"relay_list": token})
			fetched <- struct{}{}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeats") {
			_, _ = io.WriteString(w, `{"node_id":"node_`+strings.Repeat("a", 64)+`","accepted":true}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer core.Close()
	ctx, cancel := context.WithCancel(context.Background())
	holder := &relays.Holder{}
	store := &approvedStore{coreURL: core.URL, state: localstate.Connected}
	done := startFederationWorker(ctx, config{coreURL: core.URL, heartbeatInterval: 5 * time.Second,
		relayList: &relayListConfig{key: public, holder: holder}}, store, slog.New(slog.NewTextHandler(io.Discard, nil)), heartbeat.NewMonitor())
	defer func() { cancel(); <-done }()
	select {
	case <-fetched:
	case <-time.After(3 * time.Second):
		t.Fatal("relay list was not fetched after the heartbeat")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if list, ok := holder.Current(time.Now()); ok {
			if len(list.Relays) != 1 || list.Relays[0].ID != "relay-dc-1" {
				t.Fatalf("%+v", list)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("verified relay list not kept")
}
