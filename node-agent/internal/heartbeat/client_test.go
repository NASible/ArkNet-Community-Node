package heartbeat

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func identity() enrollment.Identity {
	return enrollment.Identity{NodeID: "node_" + strings.Repeat("a", 64), Status: "APPROVED", PrivateSeed: make([]byte, 32)}
}
func TestSignedMinimalHeartbeat(t *testing.T) {
	id := identity()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		if json.Unmarshal(b, &obj) != nil || len(obj) != 5 {
			t.Errorf("payload %s", b)
		}
		if r.Method != "POST" || r.URL.RequestURI() != "/api/federation/v1/nodes/"+id.NodeID+"/heartbeats" {
			t.Errorf("target %s", r.URL)
		}
		if obj["node_id"] != id.NodeID || obj["state"] != "CONNECTED" || obj["version"] != "test" {
			t.Errorf("payload %s", b)
		}
		h := obj["health"].(map[string]any)
		if len(h) != 2 || h["healthy"] != false || h["database_healthy"] != false {
			t.Errorf("health %v", h)
		}
		capabilities := obj["capabilities"].(map[string]any)
		if len(capabilities) != 1 || capabilities["peer_serving"] != false {
			t.Errorf("capabilities %v", capabilities)
		}
		msg := fmt.Sprintf("ARKNET-NODE-V1\nPOST\n%s\n%x\n%s\n%s", r.URL.EscapedPath(), sha256.Sum256(b), r.Header.Get("X-ArkNet-Node-Timestamp"), id.NodeID)
		sig, _ := base64.StdEncoding.DecodeString(r.Header.Get("X-ArkNet-Node-Signature"))
		if !ed25519.Verify(ed25519.NewKeyFromSeed(id.PrivateSeed).Public().(ed25519.PublicKey), []byte(msg), sig) {
			t.Error("signature")
		}
		fmt.Fprintf(w, `{"node_id":%q,"accepted":true}`, id.NodeID)
	}))
	defer srv.Close()
	id.CoreURL = srv.URL
	if err := send(context.Background(), id, "test", localstate.Connected, false, false); err != nil {
		t.Fatal(err)
	}
}
