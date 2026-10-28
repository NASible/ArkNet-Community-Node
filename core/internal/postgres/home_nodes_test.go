package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"arknet/core/internal/memberships"
)

func TestHomeNodeIsOneExplicitOwnMembership(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	nodeA, nodeB, nodeC := approvedNode(t, s, 41), approvedNode(t, s, 42), approvedNode(t, s, 43)
	alice, bob := newUser(t, s, "home-alice"), newUser(t, s, "home-bob")
	onA, err := s.AcceptInvitation(ctx, invite(t, s, nodeA, memberships.RoleMember, 2), alice)
	if err != nil {
		t.Fatal(err)
	}
	onB, err := s.AcceptInvitation(ctx, invite(t, s, nodeB, memberships.RoleMember, 1), alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HomeNode(ctx, alice); !errors.Is(err, memberships.ErrNoHomeNode) {
		t.Fatalf("membership implicitly set home node: %v", err)
	}

	home, err := s.SetHomeNode(ctx, alice, nodeA)
	if err != nil || home.NodeID != nodeA || home.MemberID != onA.MemberID || home.SetAt.IsZero() {
		t.Fatalf("set A: %+v %v", home, err)
	}
	home, err = s.SetHomeNode(ctx, alice, nodeB)
	if err != nil || home.NodeID != nodeB || home.MemberID != onB.MemberID {
		t.Fatalf("change to B: %+v %v", home, err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM global_user_home_nodes").Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("home rows = %d %v", rows, err)
	}
	// Joining another Node later does not change the home node.
	if _, err := s.AcceptInvitation(ctx, invite(t, s, nodeC, memberships.RoleMember, 1), alice); err != nil {
		t.Fatal(err)
	}
	if got, err := s.HomeNode(ctx, alice); err != nil || got.NodeID != nodeB || got.MemberID != onB.MemberID {
		t.Fatalf("routing lookup after new membership: %+v %v", got, err)
	}

	// Only own memberships on trusted Nodes qualify; failures are uniform.
	for name, target := range map[string]string{"not a member": nodeB, "unknown": "node_" + strings.Repeat("e", 64), "invalid": "bad"} {
		if _, err := s.SetHomeNode(ctx, bob, target); !errors.Is(err, memberships.ErrHomeNodeUnavailable) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := s.HomeNode(ctx, bob); !errors.Is(err, memberships.ErrNoHomeNode) {
		t.Fatalf("failed set created home: %v", err)
	}
	// The schema itself refuses a home row pointing at another user's membership.
	if _, err := s.db.ExecContext(ctx, `INSERT INTO global_user_home_nodes (user_id, membership_id)
SELECT b.id, m.id FROM global_users b, node_memberships m WHERE b.public_id=$1 AND m.public_id=$2`, bob, onA.MemberID); err == nil {
		t.Fatal("home row for another user's membership accepted")
	}

	// Untrusted home Node: not routable, and no fallback to A or C.
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET trust_status='BLOCKED' WHERE public_id=$1", nodeB); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HomeNode(ctx, alice); !errors.Is(err, memberships.ErrNoHomeNode) {
		t.Fatalf("blocked home node routable: %v", err)
	}
	if _, err := s.SetHomeNode(ctx, alice, nodeB); !errors.Is(err, memberships.ErrHomeNodeUnavailable) {
		t.Fatalf("set to blocked node: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET trust_status='TRUSTED' WHERE public_id=$1", nodeB); err != nil {
		t.Fatal(err)
	}
	// Disabled users have no routable home node.
	if _, err := s.db.ExecContext(ctx, "UPDATE global_users SET status='DISABLED' WHERE public_id=$1", alice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HomeNode(ctx, alice); !errors.Is(err, memberships.ErrNoHomeNode) {
		t.Fatalf("disabled user routable: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE global_users SET status='ACTIVE' WHERE public_id=$1", alice); err != nil {
		t.Fatal(err)
	}
	// Removing the home membership clears the choice instead of picking another.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM node_memberships WHERE public_id=$1", onB.MemberID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HomeNode(ctx, alice); !errors.Is(err, memberships.ErrNoHomeNode) {
		t.Fatalf("deleted membership still home: %v", err)
	}
}

func TestNodesReceiveNoHomeOrCrossNodeMembershipData(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	nodeA, nodeB := approvedNode(t, s, 51), approvedNode(t, s, 52)
	alice := newUser(t, s, "home-private")
	_, err := s.AcceptInvitation(ctx, invite(t, s, nodeA, memberships.RoleMember, 1), alice)
	if err != nil {
		t.Fatal(err)
	}
	onB, err := s.AcceptInvitation(ctx, invite(t, s, nodeB, memberships.RoleMember, 1), alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetHomeNode(ctx, alice, nodeB); err != nil {
		t.Fatal(err)
	}
	// Node A cannot resolve Alice's Node B pseudonym through refresh either.
	states, err := s.AuthorizationStates(ctx, nodeA, []string{onB.MemberID})
	if err != nil || len(states) != 1 || states[0].Authorized {
		t.Fatalf("cross-node pseudonym resolved: %+v %v", states, err)
	}
	if _, err := s.OfflineCredentials(ctx, nodeA, onB.MemberID); !errors.Is(err, memberships.ErrAuthorizationDenied) {
		t.Fatalf("cross-node credentials: %v", err)
	}
	// Home designation is stored by internal keys only: no Node or user public
	// IDs are denormalized into a table a Node-facing query could read.
	var columns string
	if err := s.db.QueryRowContext(ctx, `SELECT string_agg(column_name, ',' ORDER BY column_name) FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = 'global_user_home_nodes'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != "membership_id,updated_at,user_id" {
		t.Fatalf("home node columns = %s", columns)
	}
}
