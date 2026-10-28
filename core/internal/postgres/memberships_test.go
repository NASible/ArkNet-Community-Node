package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/users"
)

func approvedNode(t *testing.T, s *Store, n int) string {
	t.Helper()
	id := fmt.Sprintf("node_%064x", n)
	if _, err := s.Enroll(context.Background(), id, "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func invite(t *testing.T, s *Store, nodeID string, role memberships.Role, maxUses int) [32]byte {
	t.Helper()
	_, digest, _ := memberships.NewSecret()
	_, proof, _ := memberships.NewSecret()
	invitation, err := s.CreateInvitation(context.Background(), nodeID, role, time.Now().Add(time.Hour), maxUses, digest, proof)
	if err != nil {
		t.Fatal(err)
	}
	if !memberships.ValidInvitationID(invitation.ID) || invitation.NodeID != nodeID || invitation.Role != role || invitation.MaxUses != maxUses {
		t.Fatalf("invitation %+v", invitation)
	}
	return digest
}

func newUser(t *testing.T, s *Store, name string) string {
	t.Helper()
	user, err := s.CreateUser(context.Background(), name, testHash)
	if err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func usesOf(t *testing.T, s *Store, digest [32]byte) int {
	t.Helper()
	var uses int
	if err := s.db.QueryRow("SELECT uses FROM node_invitations WHERE secret_digest = $1", digest[:]).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	return uses
}

func TestInvitationCreationRequiresTrustedNodeAndRejectsReplay(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 1)
	pending := fmt.Sprintf("node_%064x", 2)
	if _, err := s.Enroll(ctx, pending, "v"); err != nil {
		t.Fatal(err)
	}
	_, digest, _ := memberships.NewSecret()
	_, proof, _ := memberships.NewSecret()
	expires := time.Now().Add(time.Hour)
	if _, err := s.CreateInvitation(ctx, pending, memberships.RoleMember, expires, 1, digest, proof); !errors.Is(err, memberships.ErrNodeNotFound) {
		t.Fatalf("pending node: %v", err)
	}
	if _, err := s.CreateInvitation(ctx, node, memberships.RoleMember, expires, 1, digest, proof); err != nil {
		t.Fatal(err)
	}
	_, other, _ := memberships.NewSecret()
	if _, err := s.CreateInvitation(ctx, node, memberships.RoleMember, expires, 1, other, proof); !errors.Is(err, memberships.ErrReplayedCreate) {
		t.Fatalf("replay: %v", err)
	}
	if _, err := s.CreateInvitation(ctx, node, "OWNER", expires, 1, other, other); !errors.Is(err, memberships.ErrInvalidRole) {
		t.Fatalf("role: %v", err)
	}
	for _, statement := range []string{
		"UPDATE node_invitations SET uses = max_uses + 1",
		"UPDATE node_invitations SET max_uses = 0",
		"UPDATE node_invitations SET role = 'OWNER'",
		"UPDATE node_invitations SET secret_digest = '\\x00'",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("schema accepted %q", statement)
		}
	}
}

func TestInvitationWorksOnceAccordingToPolicy(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 1)
	alice, bob, carol := newUser(t, s, "alice"), newUser(t, s, "bob"), newUser(t, s, "carol")

	single := invite(t, s, node, memberships.RoleAdmin, 1)
	membership, err := s.AcceptInvitation(ctx, single, alice)
	if err != nil || membership.NodeID != node || membership.Role != memberships.RoleAdmin || membership.CreatedAt.IsZero() {
		t.Fatalf("accept %+v %v", membership, err)
	}
	if _, err := s.AcceptInvitation(ctx, single, bob); !errors.Is(err, memberships.ErrUnavailable) {
		t.Fatalf("second use of single-use invitation: %v", err)
	}
	if usesOf(t, s, single) != 1 {
		t.Fatal("single-use invitation used more than once")
	}

	double := invite(t, s, node, memberships.RoleMember, 2)
	if _, err := s.AcceptInvitation(ctx, double, alice); !errors.Is(err, memberships.ErrAlreadyMember) {
		t.Fatalf("existing member: %v", err)
	}
	if usesOf(t, s, double) != 0 {
		t.Fatal("existing member consumed a use")
	}
	for _, user := range []string{bob, carol} {
		if _, err := s.AcceptInvitation(ctx, double, user); err != nil {
			t.Fatal(err)
		}
	}
	dave := newUser(t, s, "dave")
	if _, err := s.AcceptInvitation(ctx, double, dave); !errors.Is(err, memberships.ErrUnavailable) {
		t.Fatalf("exhausted: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM node_memberships").Scan(&count); err != nil || count != 3 {
		t.Fatalf("memberships = %d %v", count, err)
	}
	var role string
	if err := s.db.QueryRow("SELECT m.role FROM node_memberships m JOIN global_users u ON u.id = m.user_id WHERE u.public_id = $1", alice).Scan(&role); err != nil || role != "ADMIN" {
		t.Fatalf("alice role %q %v", role, err)
	}
	unknown := [32]byte{1}
	if _, err := s.AcceptInvitation(ctx, unknown, dave); !errors.Is(err, memberships.ErrUnavailable) {
		t.Fatalf("unknown secret: %v", err)
	}
}

func TestExpiredDisabledAndUntrustedInvitationsFail(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 1)
	alice := newUser(t, s, "alice")

	expired := invite(t, s, node, memberships.RoleMember, 5)
	if _, err := s.db.ExecContext(ctx, "UPDATE node_invitations SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 second' WHERE secret_digest = $1", expired[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvitation(ctx, expired, alice); !errors.Is(err, memberships.ErrUnavailable) {
		t.Fatalf("expired: %v", err)
	}

	open := invite(t, s, node, memberships.RoleMember, 5)
	if _, err := s.SetUserStatus(ctx, alice, users.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvitation(ctx, open, alice); !errors.Is(err, users.ErrInactive) {
		t.Fatalf("disabled user: %v", err)
	}
	if usesOf(t, s, open) != 0 {
		t.Fatal("disabled user consumed a use")
	}
	if _, err := s.SetUserStatus(ctx, alice, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET trust_status = 'BLOCKED' WHERE public_id = $1", node); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvitation(ctx, open, alice); !errors.Is(err, memberships.ErrUnavailable) {
		t.Fatalf("untrusted node: %v", err)
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM node_memberships").Scan(&count); err != nil || count != 0 {
		t.Fatalf("memberships = %d %v", count, err)
	}
}

func TestConcurrentAcceptanceNeverExceedsMaxUses(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 1)
	digest := invite(t, s, node, memberships.RoleMember, 3)
	var ids []string
	for i := 0; i < 12; i++ {
		ids = append(ids, newUser(t, s, fmt.Sprintf("user%02d", i)))
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			_, err := s.AcceptInvitation(ctx, digest, id)
			if err == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			} else if !errors.Is(err, memberships.ErrUnavailable) {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	var memberCount int
	if err := s.db.QueryRow("SELECT count(*) FROM node_memberships").Scan(&memberCount); err != nil {
		t.Fatal(err)
	}
	if accepted != 3 || memberCount != 3 || usesOf(t, s, digest) != 3 {
		t.Fatalf("accepted=%d memberships=%d uses=%d", accepted, memberCount, usesOf(t, s, digest))
	}
}
