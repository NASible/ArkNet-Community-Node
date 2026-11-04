package main

import (
	"crypto/ed25519"
	"net/url"
	"strings"
	"testing"
	"time"
)

// goldenRelayList is also verified by node-agent/internal/relays, so Core's
// signer and the Agent's verifier cannot drift apart.
const goldenRelayList = "eyJ2ZXJzaW9uIjoxLCJpc3N1ZWRfYXQiOjE3OTM3MDcyMDAsImV4cGlyZXNfYXQiOjE3OTM3OTM2MDAsInJlbGF5cyI6W3siaWQiOiJyZWxheS1kYy0xIiwiYWRkcmVzcyI6InJlbGF5MS5leGFtcGxlLm5ldDo3NDQzIiwic3BraV9zaGEyNTYiOiJhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhIn1dfQ.QiPZFITynjZ0iK0SFw5ZmnELet2yurIbb_hvqpdL3UMhHnQILYVFNbdT3O4hIuS-q04aJYUlKoqrXIQ8tyYkAw"

func TestRelayListMatchesTheAgentsGolden(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	control, _ := url.Parse("http://10.0.0.5:7080")
	cfg := &relaysConfig{listKey: ed25519.NewKeyFromSeed(seed), entries: []relayEntry{
		{ID: "relay-dc-1", Address: "relay1.example.net:7443", SPKISHA256: strings.Repeat("a", 64), ControlURL: control}}}
	token, err := signRelayList(cfg, time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	if err != nil || token != goldenRelayList {
		t.Fatalf("relay list format changed: %v\n%s", err, token)
	}
}
