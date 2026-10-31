package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"

	"arknet/core/internal/assertions"
	"arknet/core/internal/memberships"
	"arknet/core/internal/users"
)

func TestLoginCallbacksBelongToApprovedTrustedNodeAndAreReplaced(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node, other := approvedNode(t, s, 1), approvedNode(t, s, 2)
	first := []string{"https://node.example/login/callback", "http://127.0.0.1:5173/login/callback"}
	if err := s.SetLoginCallbacks(ctx, node, first); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoginCallbacks(ctx, node)
	if err != nil || fmt.Sprint(got) != fmt.Sprint(first) {
		t.Fatalf("callbacks %v %v", got, err)
	}
	if got, err := s.LoginCallbacks(ctx, other); err != nil || len(got) != 0 {
		t.Fatalf("other Node sees %v %v", got, err)
	}
	if err := s.SetLoginCallbacks(ctx, node, []string{"https://node.example/new"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LoginCallbacks(ctx, node); len(got) != 1 || got[0] != "https://node.example/new" {
		t.Fatalf("replacement %v", got)
	}
	if _, err := s.Revoke(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoginCallbacks(ctx, node); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("revoked Node callbacks: %v", err)
	}
	if err := s.SetLoginCallbacks(ctx, node, first); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("revoked Node registration: %v", err)
	}
	unknown := fmt.Sprintf("node_%064x", 99)
	if _, err := s.LoginCallbacks(ctx, unknown); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("unknown Node: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO node_login_callbacks (node_id, callback_url)
		SELECT id, $1 FROM nodes WHERE public_id = $2`, "", other); err == nil {
		t.Fatal("schema accepted an empty callback URL")
	}
}

func TestLoginMembershipIsPairwiseAndRequiresActiveUserAndTrustedNode(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node, other := approvedNode(t, s, 1), approvedNode(t, s, 2)
	alice, bob := newUser(t, s, "alice"), newUser(t, s, "bob")
	aliceOnNode, err := s.AcceptInvitation(ctx, invite(t, s, node, memberships.RoleAdmin, 5), alice)
	if err != nil {
		t.Fatal(err)
	}
	aliceOnOther, err := s.AcceptInvitation(ctx, invite(t, s, other, memberships.RoleMember, 5), alice)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.LoginMembership(ctx, node, alice)
	if err != nil || got.MemberID != aliceOnNode.MemberID || got.Role != memberships.RoleAdmin {
		t.Fatalf("alice on node: %+v %v", got, err)
	}
	if got, _ := s.LoginMembership(ctx, other, alice); got.MemberID != aliceOnOther.MemberID || got.MemberID == aliceOnNode.MemberID {
		t.Fatalf("alice on other must be her other pairwise ID: %+v", got)
	}
	if _, err := s.LoginMembership(ctx, node, bob); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("non-member: %v", err)
	}
	if _, err := s.SetUserStatus(ctx, alice, users.StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoginMembership(ctx, node, alice); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("disabled user: %v", err)
	}
	if _, err := s.SetUserStatus(ctx, alice, users.StatusActive); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Revoke(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoginMembership(ctx, node, alice); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("revoked Node: %v", err)
	}
	if _, err := s.LoginMembership(ctx, "node_bad", alice); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("malformed Node ID: %v", err)
	}
}

func TestLoginNonceIsClaimedOncePerNodeEvenConcurrently(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node, other := approvedNode(t, s, 1), approvedNode(t, s, 2)
	digest := sha256.Sum256([]byte("nonce"))
	expires := assertions.NonceRetention
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.ClaimLoginNonce(ctx, node, digest, expires) }()
	}
	wg.Wait()
	close(results)
	claimed := 0
	for err := range results {
		switch {
		case err == nil:
			claimed++
		case !errors.Is(err, assertions.ErrNonceUsed):
			t.Fatalf("claim: %v", err)
		}
	}
	if claimed != 1 {
		t.Fatalf("nonce claimed %d times", claimed)
	}
	if err := s.ClaimLoginNonce(ctx, other, digest, expires); err != nil {
		t.Fatalf("same nonce on another Node: %v", err)
	}
	if err := s.ClaimLoginNonce(ctx, fmt.Sprintf("node_%064x", 99), sha256.Sum256([]byte("x")), expires); !errors.Is(err, assertions.ErrDenied) {
		t.Fatalf("unknown Node: %v", err)
	}
	// Expired claims are pruned on the next claim; only the digest is stored.
	if _, err := s.db.ExecContext(ctx, `UPDATE node_login_nonces SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimLoginNonce(ctx, node, sha256.Sum256([]byte("fresh")), expires); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM node_login_nonces`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("expired claims not pruned: %d %v", remaining, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO node_login_nonces (node_id, nonce_digest, expires_at)
		SELECT id, '\x00'::bytea, now() + interval '1 minute' FROM nodes WHERE public_id = $1`, node); err == nil {
		t.Fatal("schema accepted a non-digest nonce")
	}
}
