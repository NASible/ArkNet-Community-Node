package postgres

import (
	"context"
	"errors"
	"testing"

	"arknet/core/internal/memberships"
)

func TestRefreshReflectsAuthority(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node := approvedNode(t, s, 31)
	user := newUser(t, s, "offline-core-user")
	membership, err := s.AcceptInvitation(ctx, invite(t, s, node, memberships.RoleAdmin, 1), user)
	if err != nil {
		t.Fatal(err)
	}
	unknown := "member_" + repeatCoreHex("f", 64)
	states, err := s.AuthorizationStates(ctx, node, []string{membership.MemberID, unknown})
	if err != nil || len(states) != 2 || !states[0].Authorized || states[0].Role != memberships.RoleAdmin || states[1].Authorized {
		t.Fatalf("%+v %v", states, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE global_users SET status='DISABLED' WHERE public_id=$1", user); err != nil {
		t.Fatal(err)
	}
	states, err = s.AuthorizationStates(ctx, node, []string{membership.MemberID})
	if err != nil || len(states) != 1 || states[0].Authorized {
		t.Fatalf("disabled refresh: %+v %v", states, err)
	}
	if _, err := s.AuthorizationStates(ctx, node, []string{membership.MemberID, membership.MemberID}); !errors.Is(err, memberships.ErrAuthorizationDenied) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func repeatCoreHex(value string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += value
	}
	return result
}
