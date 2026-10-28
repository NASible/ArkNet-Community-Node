package postgres

import (
	"context"
	"testing"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/store"
)

func TestCreateMediaObjectDefaultsToActive(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	created, err := db.CreateMediaObject(ctx, store.Object{
		ObjectType: media.ObjectTypeVideo,
		Title:      "Default active movie",
		Size:       1024,
	})
	if err != nil {
		t.Fatalf("create with omitted state: %v", err)
	}
	got, err := db.GetMediaObjectByPublicID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != media.ObjectActive || !media.ValidMediaID(got.ID) {
		t.Fatalf("created object = %+v", got)
	}
}

func TestMediaCatalogRepository(t *testing.T) {
	db := testStore(t)
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := db

	// Core, not a caller, assigns the opaque global ID.
	_, err := repo.CreateMediaObject(ctx, store.Object{
		ID:         "media_0000000000000000000000000000000000000000000000000000000000000000",
		ObjectType: media.ObjectTypeVideo,
		State:      media.ObjectActive,
	})
	if err != media.ErrInvalidMediaID {
		t.Fatalf("caller-supplied media ID: got %v, want ErrInvalidMediaID", err)
	}

	// Test 1: Create media object
	original := store.Object{
		ObjectType: media.ObjectTypeVideo,
		Title:      "Test Movie",
		Size:       1024,
		State:      media.ObjectActive,
		CreatedAt:  time.Unix(1, 0),
		UpdatedAt:  time.Unix(2, 0),
	}
	created, err := repo.CreateMediaObject(ctx, original)
	if err != nil {
		t.Fatalf("CreateMediaObject: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created media object has empty ID")
	}
	if !media.ValidMediaID(created.ID) {
		t.Fatalf("invalid media ID: %s", created.ID)
	}
	if created.CreatedAt.IsZero() || !created.CreatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("invalid creation timestamps: created=%v updated=%v", created.CreatedAt, created.UpdatedAt)
	}
	if created.CreatedAt.Equal(original.CreatedAt) || created.UpdatedAt.Equal(original.UpdatedAt) {
		t.Fatal("caller supplied Core timestamps")
	}

	// Test 2: Get media object by ID
	got, err := repo.GetMediaObjectByPublicID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetMediaObjectByPublicID: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("got ID %s, want %s", got.ID, created.ID)
	}
	if got.Title != "Test Movie" {
		t.Fatalf("got title %q, want %q", got.Title, "Test Movie")
	}

	// Test 3: List media objects
	objects, err := repo.ListMediaObjects(ctx, false, 50)
	if err != nil {
		t.Fatalf("ListMediaObjects: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("ListMediaObjects: got %d objects, want 1", len(objects))
	}

	// Test 4: Update object state - REMOVED
	removed, err := repo.UpdateMediaObjectState(ctx, created.ID, media.ObjectRemoved)
	if err != nil {
		t.Fatalf("UpdateMediaObjectState: %v", err)
	}
	if removed.State != media.ObjectRemoved {
		t.Fatalf("got state %s, want %s", removed.State, media.ObjectRemoved)
	}

	// Test 5: Removed objects should not appear in active list
	objects, err = repo.ListMediaObjects(ctx, false, 50)
	if err != nil {
		t.Fatalf("ListMediaObjects: %v", err)
	}
	if len(objects) != 0 {
		t.Fatalf("ListMediaObjects with includeRemoved=false: got %d objects, want 0", len(objects))
	}

	// Test 6: But removed should appear in full list
	objects2, err := repo.ListMediaObjects(ctx, true, 50)
	if err != nil {
		t.Fatalf("ListMediaObjects: %v", err)
	}
	if len(objects2) != 1 {
		t.Fatalf("ListMediaObjects with includeRemoved=true: got %d objects, want 1", len(objects2))
	}

	// Test 7: Not found
	_, err = repo.GetMediaObjectByPublicID(ctx, "media_0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected ErrNotFound, got nil")
	}

	// Test 8: Invalid ID
	_, err = repo.GetMediaObjectByPublicID(ctx, "invalid_id")
	if err != media.ErrInvalidMediaID {
		t.Fatalf("expected ErrInvalidMediaID, got: %v", err)
	}

	// Test 9: Update to same state
	_, err = repo.UpdateMediaObjectState(ctx, created.ID, media.ObjectRemoved)
	if err != media.ErrInvalidState {
		t.Fatalf("updating REMOVED to REMOVED: got %v, want ErrInvalidState", err)
	}
	// Verify state didn't change (still REMOVED)
	still, err := repo.GetMediaObjectByPublicID(ctx, created.ID)
	if err != nil || still.State != media.ObjectRemoved {
		t.Fatalf("state should still be REMOVED: %v", still.State)
	}

	// REMOVED is terminal: a stale or compromised node cannot reactivate it.
	if _, err := repo.UpdateMediaObjectState(ctx, created.ID, media.ObjectActive); err != media.ErrInvalidState {
		t.Fatalf("REMOVED to ACTIVE: got %v, want ErrInvalidState", err)
	}
	if _, err := repo.UpdateMediaObjectState(ctx, created.ID, media.ObjectState("DELETED")); err != media.ErrInvalidState {
		t.Fatalf("unknown state: got %v, want ErrInvalidState", err)
	}
}
