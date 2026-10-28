package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/routing"
)

type missEvent struct{ node, media string }

// blockingRecorder simulates a slow database: every record waits on release.
type blockingRecorder struct {
	mu      sync.Mutex
	events  []missEvent
	release chan struct{}
	err     error
	seen    chan missEvent
}

func (r *blockingRecorder) RecordMediaMiss(ctx context.Context, nodeID, mediaID string) error {
	r.seen <- missEvent{nodeID, mediaID}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	r.events = append(r.events, missEvent{nodeID, mediaID})
	r.mu.Unlock()
	return r.err
}

func (r *blockingRecorder) ListMediaMisses(_ context.Context, nodeID string, limit int) ([]routing.MediaMiss, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := map[string]int64{}
	for _, event := range r.events {
		if event.node == nodeID {
			counts[event.media]++
		}
	}
	misses := []routing.MediaMiss{}
	for id, count := range counts {
		misses = append(misses, routing.MediaMiss{MediaID: id, MissCount: count})
	}
	return misses, r.err
}

func missRoutingFixture(t *testing.T, recorder *blockingRecorder) (homeFixture, *routingStatuses, http.Handler) {
	t.Helper()
	f, catalog, statuses, _ := routingFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/routing/v1/media/{media_id}", mediaRoutingHandler(catalog, f.repo, f.repo, statuses, newMissDispatcher(recorder)))
	mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/media-misses", mediaMissesHandler(recorder, f.repo))
	return f, statuses, mux
}

func TestMediaMissIsRecordedWithoutDelayingCoreFallback(t *testing.T) {
	recorder := &blockingRecorder{release: make(chan struct{}), seen: make(chan missEvent, 8)}
	f, _, h := missRoutingFixture(t, recorder)
	defer close(recorder.release)
	type result struct {
		code int
		body map[string]any
		raw  string
	}
	started := time.Now()
	// Bound the request: a synchronous recorder would block here forever.
	responses := make(chan result, 1)
	go func() {
		code, body, raw := route(t, h, f.alice, missingMedia)
		responses <- result{code, body, raw}
	}()
	var got result
	select {
	case got = <-responses:
	case <-time.After(2 * time.Second):
		t.Fatal("Core fallback blocked on miss recording")
	}
	elapsed := time.Since(started)
	code, body, raw := got.code, got.body, got.raw
	if code != http.StatusOK || body["target"] != "CORE" {
		t.Fatalf("fallback: %d %s", code, raw)
	}
	// The recorder is still blocked: the response did not wait for it.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Core fallback waited for miss recording: %s", elapsed)
	}
	select {
	case event := <-recorder.seen:
		if event != (missEvent{f.node2, missingMedia}) {
			t.Fatalf("miss attributed to %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("miss not recorded")
	}
}

func TestMediaMissOnlyWhenHomeNodeLacksMedia(t *testing.T) {
	recorder := &blockingRecorder{seen: make(chan missEvent, 8)}
	f, statuses, h := missRoutingFixture(t, recorder)
	expectNone := func(label string) {
		t.Helper()
		select {
		case event := <-recorder.seen:
			t.Fatalf("%s recorded a miss: %+v", label, event)
		case <-time.After(150 * time.Millisecond):
		}
	}
	// Served by the home node: no miss.
	if _, body, _ := route(t, h, f.alice, routedMedia); body["target"] != "HOME_NODE" {
		t.Fatalf("expected home routing: %v", body)
	}
	expectNone("home hit")
	// Home offline but has the media: Core fallback, but not a media miss.
	statuses.healthy[f.node2] = false
	if _, body, _ := route(t, h, f.alice, routedMedia); body["target"] != "CORE" {
		t.Fatalf("expected core: %v", body)
	}
	expectNone("offline home with media")
	// No home node: no Node to notify.
	if _, body, _ := route(t, h, f.bob, missingMedia); body["target"] != "CORE" {
		t.Fatalf("expected core: %v", body)
	}
	expectNone("no home node")
	// Recorder errors never change the fallback response.
	recorder.err = errors.New("secret database failure")
	statuses.healthy[f.node2] = true
	if code, body, raw := route(t, h, f.alice, missingMedia); code != http.StatusOK || body["target"] != "CORE" || strings.Contains(raw, "secret") {
		t.Fatalf("recorder failure changed response: %d %s", code, raw)
	}
	if event := <-recorder.seen; event.node != f.node2 {
		t.Fatalf("miss went to non-home Node: %+v", event)
	}
}

func TestMissDispatcherDropsInsteadOfBlockingWhenSaturated(t *testing.T) {
	recorder := &blockingRecorder{release: make(chan struct{}), seen: make(chan missEvent, missDispatchConcurrency+8)}
	d := newMissDispatcher(recorder)
	for i := 0; i < missDispatchConcurrency; i++ {
		if !d.dispatch("node", "media") {
			t.Fatalf("dispatch %d dropped below capacity", i)
		}
	}
	started := time.Now()
	if d.dispatch("node", "media") {
		t.Fatal("saturated dispatcher accepted another event")
	}
	if time.Since(started) > 50*time.Millisecond {
		t.Fatal("saturated dispatch blocked")
	}
	close(recorder.release)
	var nilDispatcher *missDispatcher
	if nilDispatcher.dispatch("node", "media") {
		t.Fatal("nil dispatcher recorded")
	}
}

func TestNodeReadsOnlyItsOwnAggregatedMisses(t *testing.T) {
	recorder := &blockingRecorder{seen: make(chan missEvent, 8)}
	f, _, h := missRoutingFixture(t, recorder)
	recorder.events = []missEvent{{f.node2, missingMedia}, {f.node2, missingMedia}, {enrollmentID, routedMedia}}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, signedRequest("GET", "/api/federation/v1/nodes/"+f.node2+"/media-misses", "", f.node2, f.key2, fmt.Sprint(time.Now().Unix())))
	want := `{"misses":[{"media_id":"` + missingMedia + `","miss_count":2}]}` + "\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("node2 misses: %d %s", response.Code, response.Body)
	}
	for _, leaked := range []string{f.aliceID, f.a2, "alice", routedMedia, enrollmentID} {
		if strings.Contains(response.Body.String(), leaked) {
			t.Fatalf("miss listing leaks %q", leaked)
		}
	}
	// Another Node cannot read node2's misses; unsigned requests are refused.
	for name, request := range map[string]*http.Request{
		"cross-node": signedRequest("GET", "/api/federation/v1/nodes/"+f.node2+"/media-misses", "", enrollmentID, f.key1, fmt.Sprint(time.Now().Unix())),
		"unsigned":   httptest.NewRequest("GET", "/api/federation/v1/nodes/"+f.node2+"/media-misses", nil),
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "media_") {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body)
		}
	}
}
