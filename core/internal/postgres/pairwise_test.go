package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"arknet/core/internal/memberships"
)

func TestSameUserGetsUnrelatedMemberIDsOnDifferentNodes(t *testing.T) {
	s := migratedStore(t)
	ctx := context.Background()
	node1, node2 := approvedNode(t, s, 1), approvedNode(t, s, 2)
	userA, userB := newUser(t, s, "alice"), newUser(t, s, "bob")

	a1, err := s.AcceptInvitation(ctx, invite(t, s, node1, memberships.RoleMember, 5), userA)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.AcceptInvitation(ctx, invite(t, s, node2, memberships.RoleAdmin, 5), userA)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s.AcceptInvitation(ctx, invite(t, s, node1, memberships.RoleMember, 5), userB)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{a1.MemberID, a2.MemberID, b1.MemberID}
	for _, id := range ids {
		if !memberships.ValidMemberID(id) || strings.Contains(id, userA[5:]) || strings.Contains(id, userB[5:]) {
			t.Fatalf("member ID %q", id)
		}
	}
	if a1.MemberID == a2.MemberID || a1.MemberID == b1.MemberID || a2.MemberID == b1.MemberID {
		t.Fatalf("member IDs not pairwise unique: %v", ids)
	}
	// Unrelated: no shared prefix/suffix beyond chance for 256-bit random values.
	if a1.MemberID[7:15] == a2.MemberID[7:15] || a1.MemberID[63:] == a2.MemberID[63:] {
		t.Fatalf("same user's IDs look related: %s %s", a1.MemberID, a2.MemberID)
	}
	// Core keeps the private mapping.
	var mapped string
	if err := s.db.QueryRow(`SELECT u.public_id FROM node_memberships m JOIN global_users u ON u.id = m.user_id WHERE m.public_id = $1`, a2.MemberID).Scan(&mapped); err != nil || mapped != userA {
		t.Fatalf("mapping %q %v", mapped, err)
	}

	if _, err := s.db.ExecContext(ctx, "UPDATE node_memberships SET public_id = $1 WHERE public_id = $2", a1.MemberID, b1.MemberID); err == nil {
		t.Fatal("schema accepted a duplicate member ID")
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE node_memberships SET public_id = $1 WHERE public_id = $2", userA, b1.MemberID); err == nil {
		t.Fatal("schema accepted a user ID as member ID")
	}
}

func TestPairwiseMigrationBackfillsExistingMembershipsRandomly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0009" {
			continue
		}
		raw, _ := fs.ReadFile(migrationFiles, "migrations/"+entry.Name())
		old["migrations/"+entry.Name()] = &fstest.MapFile{Data: raw}
	}
	if err := migrate(ctx, s.db, old); err != nil {
		t.Fatal(err)
	}
	// Raw SQL: store methods read columns added by later migrations.
	node := fmt.Sprintf("node_%064x", 7)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO nodes (public_id, enrollment_status, trust_status, software_version)
		VALUES ($1, 'APPROVED', 'TRUSTED', 'v')`, node); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO global_users (public_id, username, password_hash) VALUES ($1, $2, $3)`,
			fmt.Sprintf("user_%064x", i+1), fmt.Sprintf("legacy%d", i), testHash); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO node_memberships (node_id, user_id, role)
			SELECT n.id, u.id, 'MEMBER' FROM nodes n, global_users u WHERE n.public_id = $1 AND u.username = $2`, node, fmt.Sprintf("legacy%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.public_id FROM node_memberships m JOIN nodes n ON n.id=m.node_id WHERE n.public_id=$1`, node)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var memberID string
		if err := rows.Scan(&memberID); err != nil {
			t.Fatal(err)
		}
		if !memberships.ValidMemberID(memberID) || seen[memberID] || strings.Contains(memberID, strings.Repeat("0", 16)) {
			t.Fatalf("backfilled member ID %q", memberID)
		}
		seen[memberID] = true
	}
	if err := rows.Err(); err != nil || len(seen) != 3 {
		t.Fatalf("backfilled memberships: %d %v", len(seen), err)
	}
}
