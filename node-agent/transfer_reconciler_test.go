package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

func TestReconcileCompletedRealTransferFinalizesRefreshesAndReports(t *testing.T) {
	root := t.TempDir()
	staging, mediaRoot := filepath.Join(root, "staging"), filepath.Join(root, "media")
	source := filepath.Join(staging, selectionID, selectionID)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	payload := append([]byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, []byte("payload")...)
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: int64(len(payload)), State: catalog.Active, LocalState: mediaselection.Downloading}}}
	service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: transfer.Completed, TotalBytes: int64(len(payload)), CompletedBytes: int64(len(payload)), Mock: false}}
	order := []string{}
	object := catalogTestObject()
	object.Size = int64(len(payload))
	object.InfoHashV2 = strings.Repeat("a", 64)
	r := transferReconciler{
		service: service, media: mediaOpt{repository: repo, coreURL: "https://core.example",
			fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
				return []catalog.Object{object}, nil
			}},
		stagingDir: staging, mediaDir: mediaRoot,
		refreshJellyfin: func(context.Context) error { order = append(order, "refresh"); return nil },
		reportReplica: func(context.Context, enrollment.Identity, string, int64, string) error {
			order = append(order, "report")
			return nil
		},
	}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.objects[0].LocalState != mediaselection.Available {
		t.Fatalf("state=%s", repo.objects[0].LocalState)
	}
	if strings.Join(order, ",") != "refresh,report" {
		t.Fatalf("order=%v", order)
	}
	if _, err := os.Stat(filepath.Join(mediaRoot, selectionID+".mp4")); err != nil {
		t.Fatal(err)
	}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "refresh,report,refresh,report" {
		t.Fatalf("retry order=%v", order)
	}
}

func TestReconcileRestoredPausedTransferGetsFreshGrantAndResumes(t *testing.T) {
	repo := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: 4096, State: catalog.Active, LocalState: mediaselection.Downloading}}}
	service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: transfer.Paused, TotalBytes: 4096, CompletedBytes: 1024, Mock: false}}
	object := catalogTestObject()
	object.InfoHashV2 = strings.Repeat("a", 64)
	r := transferReconciler{service: service, media: mediaOpt{repository: repo, coreURL: "https://core.example",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
			return []catalog.Object{object}, nil
		},
		grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
			return catalog.TransferGrant{TrackerURL: "https://tracker.example/announce/fresh", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + selectionID + "/fresh", TorrentURL: "https://core.example/api/torrent/v1/media/" + selectionID + "/fresh", ExpiresAt: time.Now().Add(time.Minute), Actions: []string{"download"}}, nil
		}},
	}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(service.refreshes) != 1 || strings.Join(service.actions, ",") != "resume" {
		t.Fatalf("refresh=%+v actions=%v", service.refreshes, service.actions)
	}
	if service.refreshes[0].TrackerURL == "" || service.refreshes[0].WebSeedURL == "" || service.refreshes[0].TorrentURL == "" {
		t.Fatal("fresh authorization not forwarded")
	}
}

func TestReconcileRemovedTransferCancelsWithoutTouchingRetainedFile(t *testing.T) {
	mediaDir := t.TempDir()
	path := filepath.Join(mediaDir, selectionID+".mp4")
	if err := os.WriteFile(path, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: 4096, State: catalog.Removed, LocalState: mediaselection.RemovedRetained}}}
	service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: transfer.Downloading, TotalBytes: 4096, Mock: false}}
	r := transferReconciler{service: service, media: mediaOpt{repository: repo}, mediaDir: mediaDir}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(service.actions, ",") != "cancel" {
		t.Fatalf("actions=%v", service.actions)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "retained" {
		t.Fatalf("retained file changed: %q %v", got, err)
	}
}

func TestReconcileFailureDoesNotClaimAvailability(t *testing.T) {
	repo := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: 4096, State: catalog.Active, LocalState: mediaselection.Downloading}}}
	service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: transfer.VerificationFailed, TotalBytes: 4096, ErrorCode: "verification_failed", Mock: false}}
	reported := false
	r := transferReconciler{service: service, media: mediaOpt{repository: repo, coreURL: "https://core.example"},
		refreshJellyfin: func(context.Context) error { return errors.New("must not run") },
		reportReplica:   func(context.Context, enrollment.Identity, string, int64, string) error { reported = true; return nil },
	}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if repo.objects[0].LocalState != mediaselection.Error || reported {
		t.Fatalf("state=%s reported=%v", repo.objects[0].LocalState, reported)
	}
}
