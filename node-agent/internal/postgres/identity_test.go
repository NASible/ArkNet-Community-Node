package postgres

import (
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestValidNodeID(t *testing.T) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	id := "node_" + hex.EncodeToString(random)
	if !validNodeID(id) {
		t.Fatalf("generated ID rejected: %q (length %d)", id, len(id))
	}
}
