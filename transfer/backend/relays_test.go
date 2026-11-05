package backend

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"arknet/transfer/engine"
)

type relayBuffer struct{ bytes.Buffer }

func (*relayBuffer) Close() error { return nil }

// This exercises the actual JSON-lines encoder/decoder, without claiming that
// the protocol fixture is a libtorrent transport.
func relayProtocolClient(reply string) (*Client, *relayBuffer) {
	out := new(relayBuffer)
	return &Client{in: out, out: bufio.NewReader(strings.NewReader(reply + "\n")), trackerOrigin: "https://tracker.arknet.test", webSeedOrigin: "https://core.arknet.test"}, out
}

func TestRelayAuthorizationProtocol(t *testing.T) {
	relays := []engine.Relay{{ID: "relay-1", Address: "relay.example:443", SPKISHA256: strings.Repeat("c", 64)}}
	expiry := time.Now().Add(time.Hour).Unix()
	for _, seed := range []bool{false, true} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			reply := fmt.Sprintf(`{"id":1,"status":{"object_id":%q,"state":"DOWNLOADING","total_bytes":4096,"completed_bytes":0,"mock":false,"error_code":""}}`, testID)
			if seed {
				reply = fmt.Sprintf(`{"id":1,"seed":{"object_id":%q,"state":"CHECKING","total_bytes":4096,"uploaded_bytes":0}}`, testID)
			}
			c, out := relayProtocolClient(reply)
			var err error
			if seed {
				_, err = c.SeedWithRelays(testID, 4096, testHash, seedTracker, seedTorrent, relays, expiry)
			} else {
				_, err = c.AddWithAuthorizationAndRelays(testID, 4096, testHash, seedTracker, "https://core.arknet.test/api/web-seed/v1/media/"+testID+"/YWJj.ZGVm", seedTorrent, relays, expiry)
			}
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Relays  []engine.Relay `json:"relays"`
				Expires int64          `json:"relay_list_expires_at"`
			}
			if err := json.Unmarshal(out.Bytes(), &request); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(request.Relays, relays) || request.Expires != expiry {
				t.Fatalf("lost relay authorization: %s", out.String())
			}
		})
	}
}

func TestInvalidRelayAuthorizationNeverReachesWorker(t *testing.T) {
	for _, seed := range []bool{false, true} {
		c, out := relayProtocolClient("")
		relays := []engine.Relay{{ID: "relay-1", Address: "relay.example:443", SPKISHA256: strings.Repeat("c", 64)}}
		var err error
		if seed {
			_, err = c.SeedWithRelays(testID, 4096, testHash, seedTracker, seedTorrent, relays, 0)
		} else {
			_, err = c.AddWithAuthorizationAndRelays(testID, 4096, testHash, seedTracker, "https://core.arknet.test/api/web-seed/v1/media/"+testID+"/YWJj.ZGVm", seedTorrent, relays, 0)
		}
		if !errors.Is(err, engine.ErrInvalid) || out.Len() != 0 {
			t.Fatalf("invalid list reached worker: %v %s", err, out.String())
		}
	}
}
