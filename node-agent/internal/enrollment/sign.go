package enrollment

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Identity is internal federation material, never a local HTTP response.
// JSON tags explicitly prevent accidental serialization of credentials.
type Identity struct {
	NodeID      string `json:"-"`
	CoreURL     string `json:"-"`
	Status      string `json:"-"`
	PrivateSeed []byte `json:"-"`
}

func validNodeID(id string) bool {
	if len(id) != 69 || id[:5] != "node_" {
		return false
	}
	for _, c := range id[5:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// SignRequest signs exactly the raw wire body and escaped path, not a JSON re-encoding.
func SignRequest(req *http.Request, nodeID string, seed []byte, body []byte) error {
	if req == nil || req.URL == nil || !validNodeID(nodeID) || len(seed) != ed25519.SeedSize {
		return ErrRequestFailed
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	message := fmt.Sprintf("ARKNET-NODE-V1\n%s\n%s\n%x\n%s\n%s", req.Method, req.URL.EscapedPath(), sha256.Sum256(body), ts, nodeID)
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("X-ArkNet-Node-Timestamp", ts)
	req.Header.Set("X-ArkNet-Node-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), []byte(message))))
	return nil
}
