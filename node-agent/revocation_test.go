package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

type reconcileFunc func(context.Context) error

func (f reconcileFunc) once(ctx context.Context) error { return f(ctx) }

func (m mediaOpt) withRepository(repo *selectionRepository) mediaOpt {
	m.repository = repo
	return m
}

func transferActionRecorder(h http.Handler, action string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("POST", "/api/v1/media/"+selectionID+"/transfer/"+action, "{}"))
	return w
}

func stateIs(state localstate.State) func(context.Context) (localstate.State, error) {
	return func(context.Context) (localstate.State, error) { return state, nil }
}

// A revoked Node must not ask Core for anything while it cancels.
func coreMustNotBeCalled(t *testing.T) mediaOpt {
	return mediaOpt{coreURL: "https://core.example",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
			t.Error("catalog fetched while revoked")
			return nil, errors.New("revoked")
		},
		grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
			t.Error("transfer grant requested while revoked")
			return catalog.TransferGrant{}, errors.New("revoked")
		}}
}

func TestRevokedNodeCancelsEveryActiveTransferAndKeepsLocalMedia(t *testing.T) {
	for _, state := range []transfer.State{transfer.Downloading, transfer.Paused} {
		t.Run(string(state), func(t *testing.T) {
			mediaDir := t.TempDir()
			kept := filepath.Join(mediaDir, "media_"+strings.Repeat("c", 64)+".mp4")
			if err := os.WriteFile(kept, []byte("verified"), 0o600); err != nil {
				t.Fatal(err)
			}
			repo := &selectionRepository{objects: []mediaselection.Object{
				{ID: selectionID, Size: 4096, State: catalog.Active, LocalState: mediaselection.Downloading},
				{ID: "media_" + strings.Repeat("c", 64), Size: 8, State: catalog.Active, LocalState: mediaselection.Available},
			}}
			service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: state, TotalBytes: 4096, CompletedBytes: 1024}}
			media := coreMustNotBeCalled(t)
			media.repository = repo
			r := transferReconciler{service: service, media: media, mediaDir: mediaDir, nodeState: stateIs(localstate.Revoked)}
			if err := r.once(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Join(service.actions, ",") != "cancel" || len(service.refreshes) != 0 {
				t.Fatalf("actions=%v refreshes=%d", service.actions, len(service.refreshes))
			}
			if repo.objects[0].LocalState != mediaselection.Error || repo.objects[1].LocalState != mediaselection.Available {
				t.Fatalf("states=%s,%s", repo.objects[0].LocalState, repo.objects[1].LocalState)
			}
			if got, err := os.ReadFile(kept); err != nil || string(got) != "verified" {
				t.Fatalf("verified local media changed: %q %v", got, err)
			}
		})
	}
}

func TestRevokedNodeCancelsTransfersItCannotMatchToASelection(t *testing.T) {
	service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: transfer.Downloading, TotalBytes: 4096}}
	media := coreMustNotBeCalled(t)
	media.repository = &selectionRepository{}
	r := transferReconciler{service: service, media: media, nodeState: stateIs(localstate.Revoked)}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(service.actions, ",") != "cancel" {
		t.Fatalf("actions=%v", service.actions)
	}
}

func TestRevokedNodeLeavesFinishedTransfersAlone(t *testing.T) {
	for _, state := range []transfer.State{transfer.Completed, transfer.Cancelled, transfer.VerificationFailed} {
		service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: state, TotalBytes: 4096, CompletedBytes: 4096}}
		media := coreMustNotBeCalled(t)
		media.repository = &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: 4096, State: catalog.Active, LocalState: mediaselection.Downloading}}}
		r := transferReconciler{service: service, media: media, nodeState: stateIs(localstate.Revoked),
			refreshJellyfin: func(context.Context) error { t.Error("finalized while revoked"); return nil }}
		if err := r.once(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(service.actions) != 0 {
			t.Fatalf("%s: actions=%v", state, service.actions)
		}
	}
}

func TestUnreadableNodeStateNeitherRefreshesNorCancels(t *testing.T) {
	service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, State: transfer.Downloading, TotalBytes: 4096}}
	media := coreMustNotBeCalled(t)
	media.repository = &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: 4096, State: catalog.Active, LocalState: mediaselection.Downloading}}}
	r := transferReconciler{service: service, media: media,
		nodeState: func(context.Context) (localstate.State, error) { return "", errors.New("database down") }}
	if err := r.once(context.Background()); err == nil {
		t.Fatal("expected the state error")
	}
	if len(service.actions) != 0 || len(service.refreshes) != 0 {
		t.Fatalf("actions=%v refreshes=%d", service.actions, len(service.refreshes))
	}
}

func TestRevokedNodeRefusesNewTransferActions(t *testing.T) {
	for _, tt := range []struct {
		name   string
		state  func(context.Context) (localstate.State, error)
		want   int
		reason string
	}{
		{"revoked", stateIs(localstate.Revoked), 409, "node_revoked"},
		{"state unreadable", func(context.Context) (localstate.State, error) { return "", errors.New("down") }, 503, "local_state_unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, action := range []string{"start", "resume"} {
				repo := queuedTransferRepository()
				service := &fakeTransferService{status: transfer.Status{ObjectID: selectionID, State: transfer.Paused, TotalBytes: 4096, Mock: true}}
				h := newRoutes(testRepository{}, coreMustNotBeCalled(t).withRepository(repo), transferOpt{service: service, nodeState: tt.state})
				w := transferActionRecorder(h, action)
				if w.Code != tt.want || !strings.Contains(w.Body.String(), tt.reason) {
					t.Fatalf("%s: %d %s", action, w.Code, w.Body)
				}
				if len(service.requests) != 0 || len(service.refreshes) != 0 || len(service.actions) != 0 {
					t.Fatalf("%s reached the transfer service", action)
				}
			}
		})
	}
	// Stopping stays possible: it only reduces what the Node does.
	repo := queuedTransferRepository()
	service := &fakeTransferService{status: transfer.Status{ObjectID: selectionID, State: transfer.Downloading, TotalBytes: 4096, Mock: true}}
	h := newRoutes(testRepository{}, coreMustNotBeCalled(t).withRepository(repo), transferOpt{service: service, nodeState: stateIs(localstate.Revoked)})
	for _, action := range []string{"pause", "cancel"} {
		if w := transferActionRecorder(h, action); w.Code != 200 {
			t.Fatalf("%s: %d %s", action, w.Code, w.Body)
		}
	}
}

func TestRevokedNodeMediaMissDoesNotStartATransfer(t *testing.T) {
	service := &fakeTransferService{}
	opt := transferOpt{service: service, media: coreMustNotBeCalled(t).withRepository(queuedTransferRepository()), nodeState: stateIs(localstate.Revoked)}
	if err := opt.startSelection(context.Background(), queuedTransferRepository().objects[0]); err == nil || err.Error() != "node_revoked" {
		t.Fatalf("err=%v", err)
	}
	if len(service.requests) != 0 {
		t.Fatal("transfer started while revoked")
	}
}

func TestRunReconcilerRunsAgainAsSoonAsItIsWoken(t *testing.T) {
	calls := make(chan struct{}, 4)
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := runReconciler(ctx, time.Hour, "test", reconcileFunc(func(context.Context) error { calls <- struct{}{}; return nil }), nil, wake)
	defer func() { cancel(); <-done }()
	<-calls // the immediate first pass
	wake <- struct{}{}
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("waking did not run a reconcile pass")
	}
}

func TestRevocationTransitionWakesReconcilers(t *testing.T) {
	signal := newWakeup()
	transfers, seeds := signal.channel(), signal.channel()
	observe := revocationObserver(signal)
	observe(localstate.Connected, localstate.Degraded)
	select {
	case <-transfers:
		t.Fatal("woken by a non-revocation transition")
	default:
	}
	observe(localstate.Reconnecting, localstate.Revoked)
	for _, ch := range []<-chan struct{}{transfers, seeds} {
		select {
		case <-ch:
		default:
			t.Fatal("a reconciler was not woken by REVOKED")
		}
	}
}

func TestRevokedNodeStopsAllSeeds(t *testing.T) {
	service := &fakeTransferService{seeds: map[string]transfer.SeedState{selectionID: transfer.SeedSeeding}}
	r := &seedReconciler{seeds: service, peerServing: func(context.Context) (bool, error) { return true, nil },
		nodeState: stateIs(localstate.Revoked),
		seedGrant: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
			t.Error("seed grant requested while revoked")
			return catalog.TransferGrant{}, errors.New("revoked")
		}}
	if err := r.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(service.seeds) != 0 || strings.Join(service.unseeded, ",") != selectionID {
		t.Fatalf("seeds=%v unseeded=%v", service.seeds, service.unseeded)
	}
}
