package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

var otherSelectionID = "media_" + strings.Repeat("c", 64)

// seedHarness wires a seedReconciler to fakes: local selections, Core catalog,
// Core seed grants, Peer Serving, Node state, a clock, and the transfer service.
type seedHarness struct {
	t           *testing.T
	selections  *selectionRepository
	objects     []catalog.Object
	catalogErr  error
	peerServing bool
	settingErr  error
	state       localstate.State
	grantErr    map[string]error
	grants      []string
	lifetime    time.Duration
	clock       time.Time
	service     *fakeTransferService
	reconciler  *seedReconciler
}

func newSeedHarness(t *testing.T) *seedHarness {
	h := &seedHarness{
		t:           t,
		selections:  &selectionRepository{},
		peerServing: true,
		state:       localstate.Connected,
		grantErr:    map[string]error{},
		lifetime:    10 * time.Minute,
		clock:       time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC),
		service:     &fakeTransferService{},
	}
	h.addObject(selectionID, mediaselection.Available, catalog.Active)
	media := mediaOpt{repository: h.selections, coreURL: "https://core.example",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
			return h.objects, h.catalogErr
		}}
	h.reconciler = &seedReconciler{
		seeds: h.service,
		media: media,
		peerServing: func(context.Context) (bool, error) {
			return h.peerServing, h.settingErr
		},
		nodeState: func(context.Context) (localstate.State, error) { return h.state, nil },
		seedGrant: func(_ context.Context, _ enrollment.Identity, id string) (catalog.TransferGrant, error) {
			h.grants = append(h.grants, id)
			if err := h.grantErr[id]; err != nil {
				return catalog.TransferGrant{}, err
			}
			return catalog.TransferGrant{
				TrackerURL: "https://tracker.example/announce/" + id[6:14] + ".sig",
				TorrentURL: "https://core.example/api/torrent/v1/media/" + id + "/grant.sig",
				ExpiresAt:  h.clock.Add(h.lifetime), Actions: []string{"seed"},
			}, nil
		},
		now: func() time.Time { return h.clock },
	}
	return h
}

func (h *seedHarness) addObject(id string, local mediaselection.State, state catalog.State) {
	h.selections.objects = append(h.selections.objects, mediaselection.Object{ID: id, Title: "t", Size: 4096, State: state, LocalState: local})
	h.objects = append(h.objects, catalog.Object{ID: id, Title: "t", Size: 4096, State: state, InfoHashV2: strings.Repeat("b", 64), ContentType: catalog.Video})
}

func (h *seedHarness) once() error {
	h.t.Helper()
	return h.reconciler.once(context.Background())
}

func (h *seedHarness) seeding() []string {
	h.t.Helper()
	list, err := h.service.ListSeeds(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	ids := []string{}
	for _, s := range list {
		ids = append(ids, s.ObjectID)
	}
	return ids
}

func TestPeerServingOnSeedsAvailableActiveObjectsWithSeedGrants(t *testing.T) {
	h := newSeedHarness(t)
	if err := h.once(); err != nil {
		t.Fatal(err)
	}
	if got := h.seeding(); len(got) != 1 || got[0] != selectionID {
		t.Fatalf("seeds = %v", got)
	}
	request := h.service.seedReqs[0]
	if request.ObjectID != selectionID || request.Size != 4096 || request.InfoHashV2 != strings.Repeat("b", 64) ||
		!strings.HasPrefix(request.TrackerURL, "https://tracker.example/announce/") || !strings.Contains(request.TorrentURL, "/api/torrent/v1/media/"+selectionID+"/") {
		t.Fatalf("seed request %+v", request)
	}
}

func TestSeedGrantIsRefreshedBeforeHalfItsLifetime(t *testing.T) {
	h := newSeedHarness(t)
	h.once()
	h.clock = h.clock.Add(h.lifetime/2 - time.Second)
	h.once()
	if len(h.grants) != 1 {
		t.Fatalf("refreshed too early: %d grants", len(h.grants))
	}
	h.clock = h.clock.Add(time.Second)
	h.once()
	if len(h.grants) != 2 || len(h.service.seedReqs) != 2 || h.seeding()[0] != selectionID {
		t.Fatalf("grant not refreshed at half lifetime: grants=%d seeds=%d", len(h.grants), len(h.service.seedReqs))
	}
}

func TestTurningPeerServingOffStopsAllSeedsInOneInterval(t *testing.T) {
	h := newSeedHarness(t)
	h.addObject(otherSelectionID, mediaselection.Available, catalog.Active)
	h.once()
	if len(h.seeding()) != 2 {
		t.Fatalf("seeds = %v", h.seeding())
	}
	h.peerServing = false
	if err := h.once(); err != nil {
		t.Fatal(err)
	}
	if got := h.seeding(); len(got) != 0 || len(h.service.unseeded) != 2 {
		t.Fatalf("still seeding %v after Peer Serving OFF", got)
	}
	grants := len(h.grants)
	h.once()
	if len(h.grants) != grants || len(h.service.seedReqs) != 2 {
		t.Fatal("requested seed grants while Peer Serving is OFF")
	}
}

func TestUnreadablePeerServingSettingFailsClosed(t *testing.T) {
	h := newSeedHarness(t)
	h.once()
	h.settingErr = errors.New("database unavailable")
	if err := h.once(); err == nil {
		t.Fatal("setting error not reported")
	}
	if got := h.seeding(); len(got) != 0 {
		t.Fatalf("still seeding %v with an unknown Peer Serving setting", got)
	}
}

func TestCoreRefusalStopsOnlyThatSeed(t *testing.T) {
	h := newSeedHarness(t)
	h.addObject(otherSelectionID, mediaselection.Available, catalog.Active)
	h.once()
	h.clock = h.clock.Add(h.lifetime / 2)
	h.grantErr[selectionID] = catalog.ErrGrantRefused // Core 403: no longer an eligible source
	h.once()
	if got := h.seeding(); len(got) != 1 || got[0] != otherSelectionID {
		t.Fatalf("seeds = %v, want only the still-eligible object", got)
	}
	if len(h.service.unseeded) != 1 || h.service.unseeded[0] != selectionID {
		t.Fatalf("unseeded = %v", h.service.unseeded)
	}
}

func TestTransientGrantFailureKeepsSeedOnlyUntilItsGrantExpires(t *testing.T) {
	h := newSeedHarness(t)
	h.once()
	h.grantErr[selectionID] = catalog.ErrRequestFailed // Core unreachable, not a refusal
	h.clock = h.clock.Add(h.lifetime / 2)
	h.once()
	if got := h.seeding(); len(got) != 1 {
		t.Fatalf("seed dropped while its grant is still valid: %v", got)
	}
	h.clock = h.clock.Add(h.lifetime / 2)
	h.once()
	if got := h.seeding(); len(got) != 0 {
		t.Fatalf("seed kept after its grant expired: %v", got)
	}
}

func TestNodeThatIsNotConnectedStopsAllSeeds(t *testing.T) {
	for _, state := range []localstate.State{localstate.Revoked, localstate.Quarantined, localstate.Degraded, localstate.Offline, localstate.EnrollmentPending} {
		t.Run(string(state), func(t *testing.T) {
			h := newSeedHarness(t)
			h.addObject(otherSelectionID, mediaselection.Available, catalog.Active)
			h.once()
			h.state = state
			grants := len(h.grants)
			h.once()
			if got := h.seeding(); len(got) != 0 {
				t.Fatalf("%s Node still seeding %v", state, got)
			}
			if len(h.grants) != grants {
				t.Fatalf("%s Node requested seed grants", state)
			}
		})
	}
}

func TestUnapprovedIdentityStopsAllSeeds(t *testing.T) {
	h := newSeedHarness(t)
	h.once()
	h.reconciler.media.coreURL = "https://other-core.example" // identity no longer matches: not APPROVED here
	h.once()
	if got := h.seeding(); len(got) != 0 {
		t.Fatalf("seeding without an approved identity: %v", got)
	}
}

func TestRemovedOrNoLongerAvailableMediaIsUnseeded(t *testing.T) {
	h := newSeedHarness(t)
	h.addObject(otherSelectionID, mediaselection.Available, catalog.Active)
	h.once()
	h.objects[0].State = catalog.Removed // Core tombstone
	h.selections.objects[1].LocalState = mediaselection.Error
	h.once()
	if got := h.seeding(); len(got) != 0 || len(h.service.unseeded) != 2 {
		t.Fatalf("seeds = %v unseeded = %v", got, h.service.unseeded)
	}
}

func TestNoSeedIsRequestedForMediaThatIsNotAvailable(t *testing.T) {
	h := newSeedHarness(t)
	h.selections.objects = nil
	h.objects = nil
	states := []mediaselection.State{mediaselection.NotPresent, mediaselection.Queued, mediaselection.Downloading, mediaselection.RemovedRetained, mediaselection.Error}
	for i, state := range states {
		id := "media_" + strings.Repeat(string("01234"[i]), 64)
		h.addObject(id, state, catalog.Active)
	}
	h.addObject(otherSelectionID, mediaselection.Available, catalog.Removed)
	if err := h.once(); err != nil {
		t.Fatal(err)
	}
	if len(h.grants) != 0 || len(h.service.seedReqs) != 0 {
		t.Fatalf("seed grants %v / requests %v for media that is not AVAILABLE and ACTIVE", h.grants, h.service.seedReqs)
	}
	// A catalog/selection size or hash disagreement is not a verified object either.
	h.addObject(selectionID, mediaselection.Available, catalog.Active)
	h.objects[len(h.objects)-1].InfoHashV2 = ""
	h.once()
	if len(h.grants) != 0 {
		t.Fatalf("seed grant for an object without a verified v2 hash: %v", h.grants)
	}
}

func TestCatalogFailureNeverStartsSeedsAndExpiresExistingOnes(t *testing.T) {
	h := newSeedHarness(t)
	h.catalogErr = errors.New("core offline")
	if err := h.once(); err == nil || len(h.service.seedReqs) != 0 {
		t.Fatalf("seeded without Core confirming the object: err=%v", err)
	}
	h.catalogErr = nil
	h.once()
	h.catalogErr = errors.New("core offline")
	h.clock = h.clock.Add(h.lifetime / 2)
	h.once()
	if len(h.seeding()) != 1 {
		t.Fatal("seed dropped while its grant is still valid")
	}
	h.clock = h.clock.Add(h.lifetime / 2)
	h.once()
	if len(h.seeding()) != 0 {
		t.Fatal("seed kept after its grant expired while Core was unreachable")
	}
}

func TestVerificationFailedSeedIsNotRegrantedEveryInterval(t *testing.T) {
	h := newSeedHarness(t)
	h.once()
	h.service.seeds[selectionID] = transfer.SeedVerificationFailed
	h.clock = h.clock.Add(h.lifetime)
	h.once()
	if len(h.grants) != 1 || len(h.service.seedReqs) != 1 {
		t.Fatalf("re-granted a seed whose local copy failed verification: grants=%d", len(h.grants))
	}
}
