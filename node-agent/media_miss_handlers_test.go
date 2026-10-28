package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/mediamiss"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

type memoryMisses struct {
	policy     mediamiss.Policy
	items      map[string]mediamiss.Notification
	ignored    map[string]bool
	selections *quotaSelections
}

func newMemoryMisses(selections *quotaSelections) *memoryMisses {
	return &memoryMisses{policy: mediamiss.Default, items: map[string]mediamiss.Notification{}, ignored: map[string]bool{}, selections: selections}
}
func (m *memoryMisses) MediaMissPolicy(context.Context) (mediamiss.Policy, error) {
	return m.policy, nil
}
func (m *memoryMisses) SetMediaMissPolicy(_ context.Context, p mediamiss.Policy) error {
	m.policy = p
	return nil
}
func (m *memoryMisses) StoreMediaMisses(_ context.Context, ns []mediamiss.Notification) error {
	for _, n := range ns {
		m.items[n.MediaID] = n
	}
	return nil
}
func (m *memoryMisses) PendingMediaMisses(context.Context) ([]mediamiss.Notification, error) {
	pending := []mediamiss.Notification{}
	for id, n := range m.items {
		selected := false
		for _, s := range m.selections.objects {
			selected = selected || (s.ID == id && s.LocalState != mediaselection.NotPresent)
		}
		if !m.ignored[id] && !selected {
			pending = append(pending, n)
		}
	}
	// Largest miss count first, matching the store.
	for i := range pending {
		for j := i + 1; j < len(pending); j++ {
			if pending[j].MissCount > pending[i].MissCount {
				pending[i], pending[j] = pending[j], pending[i]
			}
		}
	}
	return pending, nil
}
func (m *memoryMisses) IgnoreMediaMiss(_ context.Context, id string) error {
	if _, ok := m.items[id]; !ok || m.ignored[id] {
		return mediamiss.ErrNotFound
	}
	m.ignored[id] = true
	return nil
}

// quotaSelections enforces the allocation like the PostgreSQL store does.
type quotaSelections struct{ *selectionRepository }

func (q *quotaSelections) QueueMediaSelection(ctx context.Context, object catalog.Object, allocation int64) (mediaselection.Object, error) {
	if object.State != catalog.Active {
		return mediaselection.Object{}, mediaselection.ErrRemoved
	}
	var reserved int64
	for _, s := range q.objects {
		reserved += s.Size
	}
	if allocation != -1 && reserved+object.Size > allocation {
		return mediaselection.Object{}, mediaselection.ErrAllocationExceeded
	}
	return q.selectionRepository.QueueMediaSelection(ctx, object, allocation)
}

func missObject(id string, size int64, state catalog.State) catalog.Object {
	o := catalogTestObject()
	o.ID, o.Size, o.State, o.Title, o.InfoHashV2 = id, size, state, "Title "+id[len(id)-4:], strings.Repeat("a", 64)
	return o
}

var (
	missA = "media_" + strings.Repeat("a", 60) + "000a"
	missB = "media_" + strings.Repeat("b", 60) + "000b"
	missR = "media_" + strings.Repeat("c", 60) + "000c"
	missU = "media_" + strings.Repeat("d", 60) + "000d"
)

type missHarness struct {
	selections *quotaSelections
	misses     *memoryMisses
	media      mediaOpt
	service    *fakeTransferService
	syncer     mediaMissSyncer
	handler    http.Handler
	grants     int
}

func newMissHarness(t *testing.T, allocation int64, coreMisses []catalog.Miss) *missHarness {
	t.Helper()
	h := &missHarness{selections: &quotaSelections{&selectionRepository{}}}
	h.misses = newMemoryMisses(h.selections)
	objects := []catalog.Object{missObject(missA, 4096, catalog.Active), missObject(missB, 1024, catalog.Active), missObject(missR, 512, catalog.Removed)}
	h.media = mediaOpt{repository: h.selections, allocation: allocation, coreURL: "https://core.example",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) { return objects, nil },
		grantTransfer: func(_ context.Context, _ enrollment.Identity, id string) (catalog.TransferGrant, error) {
			h.grants++
			return catalog.TransferGrant{TrackerURL: "https://tracker.example/announce/g", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + id + "/g", TorrentURL: "https://core.example/api/torrent/v1/media/" + id + "/g"}, nil
		}}
	h.service = &fakeTransferService{real: true}
	h.syncer = mediaMissSyncer{media: h.media, repository: h.misses, transfers: transferOpt{service: h.service, media: h.media},
		fetchMisses: func(context.Context, enrollment.Identity) ([]catalog.Miss, error) { return coreMisses, nil }}
	h.handler = newHandler(testRepository{}, h.media, mediaMissOpt{h.misses}, transferOpt{service: h.service})
	return h
}

func missCall(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest(method, path, body))
	var decoded map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &decoded)
	return w, decoded
}

func pendingIDs(t *testing.T, h http.Handler) []string {
	t.Helper()
	w, body := missCall(t, h, "GET", "/api/v1/media-misses", "")
	if w.Code != 200 {
		t.Fatalf("list misses: %d %s", w.Code, w.Body)
	}
	ids := []string{}
	for _, raw := range body["notifications"].([]any) {
		ids = append(ids, raw.(map[string]any)["media_id"].(string))
	}
	return ids
}

func TestAskModeNotifiesOperatorWhoCanQueueOrIgnore(t *testing.T) {
	h := newMissHarness(t, 1<<30, []catalog.Miss{{MediaID: missA, MissCount: 3}, {MediaID: missB, MissCount: 1}, {MediaID: missR, MissCount: 9}, {MediaID: missU, MissCount: 2}})
	if w, body := missCall(t, h.handler, "GET", "/api/v1/settings/media-miss-policy", ""); w.Code != 200 || body["policy"] != "ASK" {
		t.Fatalf("default policy: %d %s", w.Code, w.Body)
	}
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Removed (R) and catalog-unknown (U) misses are never surfaced.
	if ids := pendingIDs(t, h.handler); strings.Join(ids, ",") != missA+","+missB {
		t.Fatalf("pending = %v", ids)
	}
	w, body := missCall(t, h.handler, "GET", "/api/v1/media-misses", "")
	first := body["notifications"].([]any)[0].(map[string]any)
	if body["policy"] != "ASK" || first["title"] != "Title 000a" || first["size"] != float64(4096) || first["miss_count"] != float64(3) || len(first) != 4 {
		t.Fatalf("notification view: %s", w.Body)
	}
	if len(h.selections.objects) != 0 || len(h.service.requests) != 0 {
		t.Fatal("ASK mode queued or started media on its own")
	}
	// Operator: Make Available Locally uses the normal selection path.
	if w, _ := missCall(t, h.handler, "POST", "/api/v1/media/"+missA+"/selection", "{}"); w.Code != 200 {
		t.Fatalf("queue from notification: %d %s", w.Code, w.Body)
	}
	if h.selections.objects[0].ID != missA || h.selections.objects[0].LocalState != mediaselection.Queued {
		t.Fatalf("selection = %+v", h.selections.objects)
	}
	// Operator: Ignore.
	if w, _ := missCall(t, h.handler, "POST", "/api/v1/media-misses/"+missB+"/ignore", "{}"); w.Code != http.StatusNoContent {
		t.Fatalf("ignore: %d %s", w.Code, w.Body)
	}
	if ids := pendingIDs(t, h.handler); len(ids) != 0 {
		t.Fatalf("handled notifications still pending: %v", ids)
	}
	if w, _ := missCall(t, h.handler, "POST", "/api/v1/media-misses/"+missB+"/ignore", "{}"); w.Code != http.StatusNotFound {
		t.Fatalf("repeat ignore: %d", w.Code)
	}
	// Further misses for an ignored object stay ignored.
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids := pendingIDs(t, h.handler); len(ids) != 0 {
		t.Fatalf("ignored notification resurfaced: %v", ids)
	}
}

func TestMediaMissPolicyAndRequestValidation(t *testing.T) {
	h := newMissHarness(t, 1<<30, []catalog.Miss{{MediaID: missA, MissCount: 1}})
	for _, body := range []string{`{}`, `{"policy":"ask"}`, `{"policy":"NEVER"}`, `{"policy":"ASK","x":1}`, `{"policy":1}`} {
		if w, _ := missCall(t, h.handler, "PUT", "/api/v1/settings/media-miss-policy", body); w.Code != 400 {
			t.Fatalf("%s: %d", body, w.Code)
		}
	}
	if w, body := missCall(t, h.handler, "PUT", "/api/v1/settings/media-miss-policy", `{"policy":"MANUAL"}`); w.Code != 200 || body["policy"] != "MANUAL" {
		t.Fatalf("set manual: %d %s", w.Code, w.Body)
	}
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.misses.items) != 0 || len(pendingIDs(t, h.handler)) != 0 {
		t.Fatal("MANUAL mode surfaced misses")
	}
	for _, path := range []string{"/api/v1/media-misses/bad/ignore", "/api/v1/media-misses/" + missA + "/ignore?x=1"} {
		if w, _ := missCall(t, h.handler, "POST", path, "{}"); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	if w, _ := missCall(t, h.handler, "POST", "/api/v1/media-misses/"+missA+"/ignore", `{"x":1}`); w.Code != 400 {
		t.Fatalf("ignore body: %d", w.Code)
	}
	r := selectionRequest("PUT", "/api/v1/settings/media-miss-policy", `{"policy":"ASK"}`)
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin policy change: %d", w.Code)
	}
}

func TestAutomaticModeObeysQuotaAvailabilityAndOneTransferBandwidth(t *testing.T) {
	// 2 KiB quota: missA (4 KiB, most missed) does not fit; missB (1 KiB) does.
	h := newMissHarness(t, 2048, []catalog.Miss{{MediaID: missA, MissCount: 5}, {MediaID: missB, MissCount: 1}, {MediaID: missR, MissCount: 9}})
	h.misses.policy = mediamiss.Automatic
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.selections.objects) != 1 || h.selections.objects[0].ID != missB || h.selections.objects[0].LocalState != mediaselection.Downloading {
		t.Fatalf("automatic selection = %+v", h.selections.objects)
	}
	if len(h.service.requests) != 1 || h.service.requests[0].ObjectID != missB || h.service.requests[0].TrackerURL == "" || h.grants != 1 {
		t.Fatalf("automatic start = %+v grants=%d", h.service.requests, h.grants)
	}
	// Over quota stays visible to the operator; removed never appears.
	if ids := pendingIDs(t, h.handler); strings.Join(ids, ",") != missA {
		t.Fatalf("pending after automatic = %v", ids)
	}
	// Bandwidth policy: a real transfer is in flight, so nothing else starts
	// even after the quota is raised.
	h.syncer.media.allocation = 1 << 30
	h.syncer.transfers.media.allocation = 1 << 30
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.service.requests) != 1 || len(h.selections.objects) != 1 {
		t.Fatalf("second automatic transfer started while one in flight: %+v", h.service.requests)
	}
	h.service.status.State = transfer.Completed
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.service.requests) != 2 || h.service.requests[1].ObjectID != missA {
		t.Fatalf("next automatic transfer after completion: %+v", h.service.requests)
	}
}

func TestAutomaticModeWithoutTransferEngineReservesNothing(t *testing.T) {
	h := newMissHarness(t, 1<<30, []catalog.Miss{{MediaID: missA, MissCount: 1}})
	h.misses.policy = mediamiss.Automatic
	h.syncer.transfers.service = nil
	if err := h.syncer.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.selections.objects) != 0 || len(pendingIDs(t, h.handler)) != 1 {
		t.Fatal("automatic mode reserved storage without a transfer engine")
	}
}

func TestMediaMissSyncFailsClosedWhenCoreUnavailable(t *testing.T) {
	h := newMissHarness(t, 1<<30, nil)
	h.syncer.fetchMisses = func(context.Context, enrollment.Identity) ([]catalog.Miss, error) {
		return nil, errors.New("core down")
	}
	if err := h.syncer.once(context.Background()); err == nil || len(h.misses.items) != 0 {
		t.Fatal("unavailable Core produced notifications")
	}
	// The local API keeps serving existing notifications while Core is down.
	h.misses.items[missA] = mediamiss.Notification{MediaID: missA, Title: "t", Size: 1, MissCount: 1}
	if ids := pendingIDs(t, h.handler); len(ids) != 1 {
		t.Fatalf("offline listing = %v", ids)
	}
	_ = time.Second
}
