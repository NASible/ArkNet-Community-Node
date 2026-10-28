package enrollment

import (
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

func TestSubmitPublicKeyAndSignatureVerifyRawBody(t *testing.T) {
	id := "node_" + strings.Repeat("a", 64)
	store := &memoryStore{snapshot: localstate.Snapshot{NodeID: id, State: localstate.Provisioning}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		var keyText string
		json.Unmarshal(fields["public_key"], &keyText)
		pub, err := base64.StdEncoding.DecodeString(keyText)
		if err != nil || len(pub) != 32 {
			t.Fatal("missing standard-base64 public key")
		}
		sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-ArkNet-Node-Signature"))
		if err != nil {
			t.Fatal(err)
		}
		msg := fmt.Sprintf("ARKNET-NODE-V1\nPOST\n%s\n%x\n%s\n%s", r.URL.EscapedPath(), sha256.Sum256(raw), r.Header.Get("X-ArkNet-Node-Timestamp"), id)
		if !ed25519.Verify(pub, []byte(msg), sig) {
			t.Fatal("POST signature not bound to raw body/public key")
		}
		if len(fields) != 4 {
			t.Fatal("unexpected wire fields")
		}
		json.NewEncoder(w).Encode(map[string]string{"node_id": id, "enrollment_status": "PENDING"})
	}))
	defer srv.Close()
	if err := Submit(context.Background(), srv.URL, "test", store); err != nil {
		t.Fatal(err)
	}
}
func TestCanceledStandaloneOperationFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if Submit(ctx, "", "test", nil) != ErrRequestFailed || CheckApproval(ctx, "", nil) != ErrRequestFailed {
		t.Fatal("canceled ctx ignored")
	}
}
