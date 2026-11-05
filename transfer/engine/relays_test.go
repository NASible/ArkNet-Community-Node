package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidRelays(t *testing.T) {
	now := time.Unix(1000, 0)
	good := Relay{ID: "relay-1", Address: "relay.example:443", SPKISHA256: strings.Repeat("a", 64)}
	if !ValidRelays([]Relay{good}, 1001, now) || !ValidRelays(nil, 0, now) || !ValidRelays([]Relay{}, 0, now) {
		t.Fatal("valid relay list rejected")
	}
	for _, expiry := range []int64{-1, 0, 999, 1000} {
		if ValidRelays([]Relay{good}, expiry, now) {
			t.Fatalf("accepted expiry %d", expiry)
		}
	}
	if ValidRelays(nil, 1001, now) {
		t.Fatal("orphan expiry accepted")
	}
	for _, address := range []string{"relay.example:443", "203.0.113.10:443", "[2001:db8::1]:443"} {
		valid := good
		valid.ID = "relay.dc-1"
		valid.Address = address
		if !ValidRelays([]Relay{valid}, 1001, now) {
			t.Errorf("canonical relay endpoint rejected: %q", address)
		}
	}
	for _, address := range []string{"relay.example", "https://relay.example:443", "relay.example:0443", "relay.example:0", "relay.example:65536", "Relay.example:443", "relay..example:443", "relay.example.:443", "100.64.0.1:443", "[::ffff:100.64.0.1]:443", "relay.example:443/path", "-relay.example:443", "relay_.example:443"} {
		bad := good
		bad.Address = address
		if ValidRelays([]Relay{bad}, 1001, now) {
			t.Errorf("accepted address %q", address)
		}
	}
	for _, id := range []string{"", "Relay", strings.Repeat("a", 65), "relay/1"} {
		bad := good
		bad.ID = id
		if ValidRelays([]Relay{bad}, 1001, now) {
			t.Errorf("accepted ID %q", id)
		}
	}
	for _, pin := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		bad := good
		bad.SPKISHA256 = pin
		if ValidRelays([]Relay{bad}, 1001, now) {
			t.Errorf("accepted pin %q", pin)
		}
	}
	for _, field := range []string{"id", "address", "pin"} {
		other := Relay{ID: "relay-2", Address: "other.example:443", SPKISHA256: strings.Repeat("b", 64)}
		switch field {
		case "id":
			other.ID = good.ID
		case "address":
			other.Address = good.Address
		case "pin":
			other.SPKISHA256 = good.SPKISHA256
		}
		if ValidRelays([]Relay{good, other}, 1001, now) {
			t.Errorf("accepted duplicate %s", field)
		}
	}
	var list []Relay
	for i := 0; i < 17; i++ {
		list = append(list, Relay{ID: fmt.Sprintf("relay-%d", i), Address: fmt.Sprintf("relay%d.example:443", i), SPKISHA256: fmt.Sprintf("%064x", i)})
	}
	if !ValidRelays(list[:16], 1001, now) || ValidRelays(list, 1001, now) {
		t.Fatal("relay count bound not enforced")
	}
}
