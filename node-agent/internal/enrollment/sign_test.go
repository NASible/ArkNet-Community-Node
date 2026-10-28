package enrollment

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestSignRequestExactWireMessage(t *testing.T) {
	seed := make([]byte, 32)
	id := "node_" + strings.Repeat("a", 64)
	req, _ := http.NewRequest("POST", "https://core.example/a%2Fb", nil)
	body := []byte(`{"raw":true}`)
	if err := SignRequest(req, id, seed, body); err != nil {
		t.Fatal(err)
	}
	ts := req.Header.Get("X-ArkNet-Node-Timestamp")
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != ts {
		t.Fatal("noncanonical timestamp")
	}
	sig, err := base64.StdEncoding.DecodeString(req.Header.Get("X-ArkNet-Node-Signature"))
	if err != nil || len(sig) != 64 {
		t.Fatal("invalid signature encoding")
	}
	msg := fmt.Sprintf("ARKNET-NODE-V1\nPOST\n/a%%2Fb\n%x\n%s\n%s", sha256.Sum256(body), ts, id)
	if !ed25519.Verify(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey), []byte(msg), sig) {
		t.Fatal("wrong signed bytes")
	}
	for _, seed := range [][]byte{nil, make([]byte, 31), make([]byte, 64)} {
		if SignRequest(req, id, seed, body) == nil {
			t.Fatal("invalid seed accepted")
		}
	}
	if SignRequest(req, "node_bad", make([]byte, 32), body) == nil {
		t.Fatal("invalid id accepted")
	}
}
