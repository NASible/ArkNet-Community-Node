package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"arknet/core/internal/nodes"
)

func TestNodeCapabilitiesDefaultToBothAndAreReadOnEveryPath(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	id := "node_" + strings.Repeat("e", 64)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	registered, err := s.RegisterEnrollment(ctx, id, "v1", pub)
	if err != nil {
		t.Fatal(err)
	}
	want := "memberships,swarm"
	join := func(c []nodes.Capability) string {
		out := make([]string, len(c))
		for i, v := range c {
			out[i] = string(v)
		}
		return strings.Join(out, ",")
	}
	if join(registered.Capabilities) != want {
		t.Fatalf("registered capabilities %v", registered.Capabilities)
	}
	if node, _, err := s.EnrollmentIdentity(ctx, id); err != nil || join(node.Capabilities) != want {
		t.Fatalf("identity capabilities %v %v", node.Capabilities, err)
	}
	if node, err := s.Approve(ctx, id); err != nil || join(node.Capabilities) != want {
		t.Fatalf("approve capabilities %v %v", node.Capabilities, err)
	}

	// Narrowing is durable and visible to the authentication path.
	node, err := s.SetCapabilities(ctx, id, []nodes.Capability{nodes.CapabilitySwarm})
	if err != nil || join(node.Capabilities) != "swarm" {
		t.Fatalf("set: %v %v", node.Capabilities, err)
	}
	if node, _, _ := s.EnrollmentIdentity(ctx, id); join(node.Capabilities) != "swarm" {
		t.Fatalf("identity after set %v", node.Capabilities)
	}
	if node, err := s.SetCapabilities(ctx, id, []nodes.Capability{}); err != nil || len(node.Capabilities) != 0 {
		t.Fatalf("base-only: %v %v", node.Capabilities, err)
	}
	if node, err := s.GetByPublicID(ctx, id); err != nil || len(node.Capabilities) != 0 {
		t.Fatalf("metadata after base-only: %v %v", node.Capabilities, err)
	}
	if node, err := s.Revoke(ctx, id); err != nil || len(node.Capabilities) != 0 {
		t.Fatalf("revoke path: %v %v", node.Capabilities, err)
	}
	if _, err := s.SetCapabilities(ctx, "node_"+strings.Repeat("f", 64), nil); !errors.Is(err, nodes.ErrNotFound) {
		t.Fatalf("unknown node: %v", err)
	}
	if _, err := s.SetCapabilities(ctx, id, []nodes.Capability{"root"}); !errors.Is(err, nodes.ErrInvalidCapabilities) {
		t.Fatalf("unknown capability: %v", err)
	}
	for _, statement := range []string{
		"UPDATE nodes SET capabilities=ARRAY['root'] WHERE public_id='" + id + "'",
		"UPDATE nodes SET capabilities=ARRAY['swarm','swarm'] WHERE public_id='" + id + "'",
		"UPDATE nodes SET capabilities=NULL WHERE public_id='" + id + "'",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted: %s", statement)
		}
	}
}
