package transfer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRelayRequestWireAndValidation(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	entry := func(id, address, pin string) string {
		b, _ := json.Marshal(map[string]string{"id": id, "address": address, "spki_sha256": pin})
		return string(b)
	}
	unique := make([]string, 17)
	for i := range unique {
		unique[i] = entry(fmt.Sprintf("relay-%d", i), fmt.Sprintf("relay-%d.example:443", i), fmt.Sprintf("%064x", i))
	}
	good := entry("relay-1", "relay.example:443", strings.Repeat("a", 64))
	cases := []struct {
		name, entries string
		expiry        int64
		valid         bool
	}{
		{"valid", good, future, true},
		{"absent", "", 0, true},
		{"expired", good, time.Now().Unix(), false},
		{"missing expiry", good, 0, false},
		{"orphan expiry", "", future, false},
		{"duplicate id", good + "," + entry("relay-1", "other.example:443", strings.Repeat("b", 64)), future, false},
		{"duplicate pin", good + "," + entry("relay-2", "other.example:443", strings.Repeat("a", 64)), future, false},
		{"duplicate address", good + "," + entry("relay-2", "relay.example:443", strings.Repeat("b", 64)), future, false},
		{"sixteen", strings.Join(unique[:16], ","), future, true},
		{"too many", strings.Join(unique, ","), future, false},
		{"ipv4", entry("relay-1", "192.0.2.1:443", strings.Repeat("a", 64)), future, true},
		{"ipv6", entry("relay-1", "[2001:db8::1]:443", strings.Repeat("a", 64)), future, true},
	}
	for _, address := range []string{"relay.example", "https://relay.example:443", "relay.example:0443", "relay.example:0", "relay.example:65536", "Relay.example:443", "relay..example:443", "relay.example.:443", "100.64.0.1:443", "[::ffff:100.64.0.1]:443", "relay.example:443/path"} {
		cases = append(cases, struct {
			name, entries string
			expiry        int64
			valid         bool
		}{"address " + address, entry("relay-1", address, strings.Repeat("a", 64)), future, false})
	}
	for _, pin := range []string{"", strings.Repeat("A", 64), strings.Repeat("a", 63)} {
		cases = append(cases, struct {
			name, entries string
			expiry        int64
			valid         bool
		}{"pin " + pin, entry("relay-1", "relay.example:443", pin), future, false})
	}
	for _, id := range []string{"", "Relay", strings.Repeat("a", 65)} {
		cases = append(cases, struct {
			name, entries string
			expiry        int64
			valid         bool
		}{"id " + id, entry(id, "relay.example:443", strings.Repeat("a", 64)), future, false})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"object_id":%q,"size":10,"info_hash_v2":%q,"tracker_url":"https://tracker.example/announce/grant","web_seed_url":"https://core.example/web-seed","torrent_url":%q,"relays":[%s],"relay_list_expires_at":%d}`, testID, strings.Repeat("a", 64), "https://core.example/api/torrent/v1/media/"+testID+"/grant", tc.entries, tc.expiry)
			var r Request
			var s SeedRequest
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(raw), &s); err != nil {
				t.Fatal(err)
			}
			if validRequest(r) != tc.valid || validSeedRequest(s) != tc.valid {
				t.Fatalf("request/seed validity = %v/%v, want %v", validRequest(r), validSeedRequest(s), tc.valid)
			}
			if tc.name == "valid" {
				for _, v := range []any{r, s} {
					b, _ := json.Marshal(v)
					if !strings.Contains(string(b), `"relays":[{"id":"relay-1","address":"relay.example:443","spki_sha256":"`+strings.Repeat("a", 64)+`"}]`) || !strings.Contains(string(b), fmt.Sprintf(`"relay_list_expires_at":%d`, future)) {
						t.Fatalf("relay fields lost: %s", b)
					}
				}
			}
		})
	}
}
