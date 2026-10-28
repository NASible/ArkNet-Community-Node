package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

type fakeTransferService struct {
	status    transfer.Status
	requests  []transfer.Request
	refreshes []transfer.Request
	actions   []string
	listErr   error
	real      bool
	// Peer Serving (transfer.Seeder) state.
	seeds    map[string]transfer.SeedState
	seedReqs []transfer.SeedRequest
	unseeded []string
	seedErr  error
}

var _ transfer.Seeder = (*fakeTransferService)(nil)

func (s *fakeTransferService) Seed(_ context.Context, r transfer.SeedRequest) (transfer.SeedStatus, error) {
	s.seedReqs = append(s.seedReqs, r)
	if s.seedErr != nil {
		return transfer.SeedStatus{}, s.seedErr
	}
	if s.seeds == nil {
		s.seeds = map[string]transfer.SeedState{}
	}
	if _, ok := s.seeds[r.ObjectID]; !ok {
		s.seeds[r.ObjectID] = transfer.SeedChecking
	}
	return transfer.SeedStatus{ObjectID: r.ObjectID, State: s.seeds[r.ObjectID]}, nil
}
func (s *fakeTransferService) Unseed(_ context.Context, id string) (transfer.SeedStatus, error) {
	if _, ok := s.seeds[id]; !ok {
		return transfer.SeedStatus{}, transfer.ErrNotFound
	}
	delete(s.seeds, id)
	s.unseeded = append(s.unseeded, id)
	return transfer.SeedStatus{ObjectID: id, State: transfer.SeedRemoved}, nil
}
func (s *fakeTransferService) ListSeeds(context.Context) ([]transfer.SeedStatus, error) {
	if s.seedErr != nil {
		return nil, s.seedErr
	}
	out := []transfer.SeedStatus{}
	for id, state := range s.seeds {
		out = append(out, transfer.SeedStatus{ObjectID: id, State: state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out, nil
}

func (s *fakeTransferService) Add(_ context.Context, r transfer.Request) (transfer.Status, error) {
	s.requests = append(s.requests, r)
	s.status = transfer.Status{ObjectID: r.ObjectID, State: transfer.Downloading, TotalBytes: r.Size, Mock: !s.real}
	return s.status, nil
}
func (s *fakeTransferService) List(context.Context) ([]transfer.Status, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.status.ObjectID == "" {
		return []transfer.Status{}, nil
	}
	return []transfer.Status{s.status}, nil
}
func (s *fakeTransferService) Get(context.Context, string) (transfer.Status, error) {
	return s.status, nil
}
func (s *fakeTransferService) Pause(context.Context, string) (transfer.Status, error) {
	s.actions = append(s.actions, "pause")
	s.status.State = transfer.Paused
	return s.status, nil
}
func (s *fakeTransferService) Resume(context.Context, string) (transfer.Status, error) {
	s.actions = append(s.actions, "resume")
	s.status.State = transfer.Downloading
	return s.status, nil
}
func (s *fakeTransferService) Refresh(_ context.Context, r transfer.Request) (transfer.Status, error) {
	s.refreshes = append(s.refreshes, r)
	return s.status, nil
}
func (s *fakeTransferService) Cancel(context.Context, string) (transfer.Status, error) {
	s.actions = append(s.actions, "cancel")
	s.status.State = transfer.Cancelled
	return s.status, nil
}

func transferTestHandler(repo *selectionRepository, service transfer.Service, object catalog.Object, fetchErr error) http.Handler {
	return newHandler(testRepository{}, mediaOpt{repository: repo, allocation: 8192, coreURL: "https://core.example", fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
		return []catalog.Object{object}, fetchErr
	}, grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
		return catalog.TransferGrant{TrackerURL: "https://tracker.example/announce/grant", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + selectionID + "/grant", TorrentURL: "https://core.example/api/torrent/v1/media/" + selectionID + "/grant"}, nil
	}}, transferOpt{service: service})
}
func queuedTransferRepository() *selectionRepository {
	return &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Title: "Selected", Size: 4096, State: catalog.Active, LocalState: mediaselection.Queued}}}
}
func transferAction(t *testing.T, h http.Handler, action string, want int) transfer.Status {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("POST", "/api/v1/media/"+selectionID+"/transfer/"+action, "{}"))
	if w.Code != want {
		t.Fatalf("%s: %d %s", action, w.Code, w.Body)
	}
	var result transfer.Status
	if want == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
func TestAgentDrivesMockTransferLifecycle(t *testing.T) {
	repo := queuedTransferRepository()
	service := &fakeTransferService{}
	h := transferTestHandler(repo, service, catalogTestObject(), nil)
	start := transferAction(t, h, "start", 200)
	if !start.Mock || start.State != transfer.Downloading || len(service.requests) != 1 || service.requests[0].Size != 4096 {
		t.Fatalf("start: %+v requests=%+v", start, service.requests)
	}
	service.status.CompletedBytes = 1024
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("GET", "/api/v1/transfers", ""))
	var listing transferListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &listing) != nil || !listing.Enabled || !listing.Available || len(listing.Transfers) != 1 || listing.Transfers[0].CompletedBytes != 1024 {
		t.Fatalf("progress: %d %s", w.Code, w.Body)
	}
	if transferAction(t, h, "pause", 200).State != transfer.Paused {
		t.Fatal("not paused")
	}
	if transferAction(t, h, "resume", 200).State != transfer.Downloading {
		t.Fatal("not resumed")
	}
	if transferAction(t, h, "cancel", 200).State != transfer.Cancelled {
		t.Fatal("not cancelled")
	}
	// Synthetic completion/failure are reports, never proof of a local media file.
	for _, state := range []transfer.State{transfer.Completed, transfer.VerificationFailed} {
		service.status.State = state
		service.status.CompletedBytes = service.status.TotalBytes
		if state == transfer.VerificationFailed {
			service.status.ErrorCode = "verification_failed"
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, selectionRequest("GET", "/api/v1/transfers", ""))
		if w.Code != 200 || !strings.Contains(w.Body.String(), string(state)) {
			t.Fatalf("report: %d %s", w.Code, w.Body)
		}
		if repo.objects[0].LocalState != mediaselection.Queued {
			t.Fatal("mock report changed real local media availability")
		}
	}
}

func TestAgentTransferAuthorizationBeforeDispatch(t *testing.T) {
	for _, tt := range []struct {
		name     string
		repo     *selectionRepository
		object   catalog.Object
		fetchErr error
		want     int
	}{
		{"not selected", &selectionRepository{}, catalogTestObject(), nil, 404},
		{"removed locally", &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Size: 4096, State: catalog.Removed, LocalState: mediaselection.RemovedRetained}}}, catalogTestObject(), nil, 409},
		{"removed at Core", queuedTransferRepository(), catalog.Object{ID: selectionID, State: catalog.Removed}, nil, 409},
		{"Core unavailable", queuedTransferRepository(), catalogTestObject(), errors.New("secret Core URL"), 503},
		{"changed reservation", queuedTransferRepository(), func() catalog.Object { o := catalogTestObject(); o.Size = 8192; return o }(), nil, 409},
	} {
		t.Run(tt.name, func(t *testing.T) {
			service := &fakeTransferService{}
			h := transferTestHandler(tt.repo, service, tt.object, tt.fetchErr)
			transferAction(t, h, "start", tt.want)
			if len(service.requests) != 0 {
				t.Fatal("unauthorized object reached transfer service")
			}
		})
	}
}
func TestAgentCanStopTransferWithoutCore(t *testing.T) {
	repo := queuedTransferRepository()
	service := &fakeTransferService{status: transfer.Status{ObjectID: selectionID, State: transfer.Downloading, TotalBytes: 4096, Mock: true}}
	h := transferTestHandler(repo, service, catalogTestObject(), errors.New("offline"))
	transferAction(t, h, "pause", 200)
	transferAction(t, h, "resume", 503)
	transferAction(t, h, "cancel", 200)
	if strings.Join(service.actions, ",") != "pause,cancel" {
		t.Fatalf("actions=%v", service.actions)
	}
}
func TestAgentTransferDisabledAndUnavailableDoNotBreakMedia(t *testing.T) {
	for _, service := range []transfer.Service{nil, &fakeTransferService{listErr: errors.New("private token")}} {
		repo := queuedTransferRepository()
		h := transferTestHandler(repo, service, catalogTestObject(), nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, selectionRequest("GET", "/api/v1/transfers", ""))
		var result transferListResponse
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Available || strings.Contains(w.Body.String(), "private") {
			t.Fatalf("unavailable: %d %s", w.Code, w.Body)
		}
		w = httptest.NewRecorder()
		h.ServeHTTP(w, selectionRequest("GET", "/api/v1/media", ""))
		if w.Code != 200 {
			t.Fatalf("media broken by transfer config: %d", w.Code)
		}
	}
}
func TestAgentTransferListFiltersUnselectedObjects(t *testing.T) {
	service := &fakeTransferService{status: transfer.Status{ObjectID: "media_" + strings.Repeat("b", 64), State: transfer.Downloading, TotalBytes: 1, Mock: true}}
	w := httptest.NewRecorder()
	transferTestHandler(queuedTransferRepository(), service, catalogTestObject(), nil).ServeHTTP(w, selectionRequest("GET", "/api/v1/transfers", ""))
	if w.Code != 200 || strings.Contains(w.Body.String(), service.status.ObjectID) {
		t.Fatalf("unselected transfer leaked: %d %s", w.Code, w.Body)
	}
}
func TestAgentTransferMutationsRejectUntrustedInput(t *testing.T) {
	for _, tt := range []struct {
		body, origin string
		want         int
	}{
		{`{"size":1}`, "", 400}, {"null", "", 400}, {"{}{}", "", 400}, {"{}", "https://evil.example", 403},
	} {
		service := &fakeTransferService{}
		h := transferTestHandler(queuedTransferRepository(), service, catalogTestObject(), nil)
		r := selectionRequest("POST", "/api/v1/media/"+selectionID+"/transfer/start", tt.body)
		r.Header.Set("Origin", tt.origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tt.want || len(service.requests) != 0 {
			t.Fatalf("untrusted input status=%d", w.Code)
		}
	}
}

func TestRealStartReceivesContentWhilePeerServingIsOff(t *testing.T) {
	repo := queuedTransferRepository()
	service := &fakeTransferService{real: true}
	media := mediaOpt{repository: repo, allocation: 8192, coreURL: "https://core.example",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
			o := catalogTestObject()
			o.InfoHashV2 = strings.Repeat("a", 64)
			return []catalog.Object{o}, nil
		},
		grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
			return catalog.TransferGrant{TrackerURL: "https://tracker.example/announce/grant", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + selectionID + "/grant", TorrentURL: "https://core.example/api/torrent/v1/media/" + selectionID + "/grant"}, nil
		},
	}
	settings := &testSettings{peerServing: false}
	h := newHandler(testRepository{}, media, transferOpt{service: service}, settingsOpt{settings})
	status := transferAction(t, h, "start", 200)
	if status.Mock || len(service.requests) != 1 || service.requests[0].TrackerURL == "" || service.requests[0].WebSeedURL == "" || repo.objects[0].LocalState != mediaselection.Downloading {
		t.Fatalf("status=%+v requests=%+v selection=%+v", status, service.requests, repo.objects[0])
	}
}

func TestFakeTransferServiceSeederLifecycle(t *testing.T) {
	s := &fakeTransferService{}
	request := transfer.SeedRequest{ObjectID: selectionID, Size: 4096, InfoHashV2: strings.Repeat("a", 64)}
	if got, err := s.Seed(context.Background(), request); err != nil || got.State != transfer.SeedChecking {
		t.Fatalf("seed %#v %v", got, err)
	}
	if list, _ := s.ListSeeds(context.Background()); len(list) != 1 || list[0].ObjectID != selectionID {
		t.Fatalf("list %#v", list)
	}
	if got, err := s.Unseed(context.Background(), selectionID); err != nil || got.State != transfer.SeedRemoved {
		t.Fatalf("unseed %#v %v", got, err)
	}
	if _, err := s.Unseed(context.Background(), selectionID); !errors.Is(err, transfer.ErrNotFound) {
		t.Fatalf("second unseed %v", err)
	}
	if list, _ := s.ListSeeds(context.Background()); list == nil || len(list) != 0 {
		t.Fatalf("empty list %#v", list)
	}
}
