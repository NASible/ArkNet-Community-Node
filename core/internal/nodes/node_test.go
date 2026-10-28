package nodes

import (
	"regexp"
	"testing"
)

func TestPublicIDsAreOpaqueAndDistinct(t *testing.T) {
	format := regexp.MustCompile(`^node_[0-9a-f]{64}$`)
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id, err := NewPublicID()
		if err != nil {
			t.Fatal(err)
		}
		if !format.MatchString(id) || !ValidPublicID(id) {
			t.Fatalf("invalid ID: %q", id)
		}
		if seen[id] {
			t.Fatal("duplicate random ID")
		}
		seen[id] = true
	}
}

func TestRejectsIdentifyingOrMalformedPublicIDs(t *testing.T) {
	for _, id := range []string{"", "operator@example.com", "node_alaska-host", "node_123", "NODE_123"} {
		if ValidPublicID(id) {
			t.Fatalf("accepted %q", id)
		}
	}
}
