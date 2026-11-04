package relays

import (
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

// Same token as core/relay_list_golden_test.go (signed by Core's signer).
const goldenRelayList = "eyJ2ZXJzaW9uIjoxLCJpc3N1ZWRfYXQiOjE3OTM3MDcyMDAsImV4cGlyZXNfYXQiOjE3OTM3OTM2MDAsInJlbGF5cyI6W3siaWQiOiJyZWxheS1kYy0xIiwiYWRkcmVzcyI6InJlbGF5MS5leGFtcGxlLm5ldDo3NDQzIiwic3BraV9zaGEyNTYiOiJhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhIn1dfQ.QiPZFITynjZ0iK0SFw5ZmnELet2yurIbb_hvqpdL3UMhHnQILYVFNbdT3O4hIuS-q04aJYUlKoqrXIQ8tyYkAw"

func TestCoreSignedGoldenListVerifies(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	key := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	list, err := Verify(goldenRelayList, key, time.Date(2026, 11, 3, 13, 0, 0, 0, time.UTC))
	if err != nil || len(list.Relays) != 1 || list.Relays[0] != (Relay{ID: "relay-dc-1", Address: "relay1.example.net:7443",
		SPKISHA256: strings.Repeat("a", 64)}) || !list.ExpiresAt.Equal(time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", list, err)
	}
}
