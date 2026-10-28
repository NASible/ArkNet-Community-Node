package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"arknet/core/internal/nodes"
)

func TestListPendingFiltersOrdersAndLooksAhead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var want []string
	// Insert in reverse order to ensure order is by public ID, not internal ID.
	for i := 105; i >= 1; i-- {
		id := fmt.Sprintf("node_%064x", i)
		if _, err := s.Enroll(ctx, id, "original"); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	sort.Strings(want)
	for i, state := range [][2]string{{"APPROVED", "TRUSTED"}, {"REJECTED", "BLOCKED"}, {"PENDING", "TRUSTED"}, {"APPROVED", "UNTRUSTED"}, {"REJECTED", "UNTRUSTED"}, {"PENDING", "BLOCKED"}} {
		id := fmt.Sprintf("node_%064x", 200+i)
		if _, err := s.Enroll(ctx, id, "filtered"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE nodes SET enrollment_status=$2, trust_status=$3 WHERE public_id=$1", id, state[0], state[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, limit := range []int{1, 2, 100} {
		got, err := s.ListPending(ctx, "", limit)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, node := range got {
			if node.EnrollmentStatus != nodes.EnrollmentPending || node.TrustStatus != nodes.TrustUntrusted || node.ID <= 0 || node.SoftwareVersion != "original" || node.CreatedAt.IsZero() {
				t.Fatalf("invalid pending node: %+v", node)
			}
			ids = append(ids, node.PublicID)
		}
		if !reflect.DeepEqual(ids, want[:limit+1]) {
			t.Fatalf("limit %d: got %v", limit, ids)
		}
	}
	var collected []string
	after := ""
	for {
		page, err := s.ListPending(ctx, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		hasMore := len(page) > 2
		if hasMore {
			page = page[:2]
		}
		for _, node := range page {
			collected = append(collected, node.PublicID)
		}
		if !hasMore {
			break
		}
		after = page[len(page)-1].PublicID
	}
	if !reflect.DeepEqual(collected, want) {
		t.Fatalf("paging: got %v", collected)
	}
	// Cursors need not identify an existing row.
	for _, after := range []string{fmt.Sprintf("node_%064x", 106), fmt.Sprintf("node_%064x", 999)} {
		got, err := s.ListPending(ctx, after, 100)
		if err != nil || len(got) != 0 {
			t.Fatalf("end cursor: nodes=%v err=%v", got, err)
		}
	}
}

func TestListPendingInvalidArgumentsDoNotAccessDatabase(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	for _, limit := range []int{-1, 0, 101} {
		if _, err := s.ListPending(ctx, "", limit); err != nodes.ErrInvalidPage {
			t.Fatalf("limit %d: error = %v", limit, err)
		}
	}
	for _, after := range []string{"operator@example.com", " node_" + strings.Repeat("a", 64), "node_" + strings.Repeat("A", 64), "node_" + strings.Repeat("a", 63)} {
		if _, err := s.ListPending(ctx, after, 1); !errors.Is(err, nodes.ErrInvalidPublicID) || strings.Contains(err.Error(), after) {
			t.Fatalf("invalid cursor: error = %v", err)
		}
	}
}
