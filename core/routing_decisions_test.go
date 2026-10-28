package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/routing"
	"arknet/core/internal/store"
)

type routingCatalog struct {
	objects map[string]store.Object
	reads   int
}

func (c *routingCatalog) CreateMediaObject(context.Context, store.Object) (store.Object, error) {
	return store.Object{}, errors.New("unused")
}
func (c *routingCatalog) GetMediaObjectByPublicID(_ context.Context, id string) (store.Object, error) {
	c.reads++
	object, ok := c.objects[id]
	if !ok {
		return store.Object{}, store.ErrNotFound
	}
	return object, nil
}
func (c *routingCatalog) ListMediaObjects(context.Context, bool, int) ([]store.Object, error) {
	return nil, errors.New("unused")
}
func (c *routingCatalog) UpdateMediaObjectState(context.Context, string, store.ObjectState) (store.Object, error) {
	return store.Object{}, errors.New("unused")
}

type routingStatuses struct {
	healthy   map[string]bool            // node -> healthy
	available map[string]map[string]bool // node -> media -> verified
	err       error
	calls     []string
}

func (s *routingStatuses) HomeNodeStatus(_ context.Context, nodeID, mediaID string, maxAge time.Duration) (routing.HomeNodeStatus, error) {
	s.calls = append(s.calls, nodeID)
	if maxAge != routing.MaxHeartbeatAge {
		return routing.HomeNodeStatus{}, errors.New("unexpected freshness bound")
	}
	return routing.HomeNodeStatus{Healthy: s.healthy[nodeID], VerifiedAvailable: s.available[nodeID][mediaID]}, s.err
}

const (
	routedMedia  = "media_" + "1111111111111111111111111111111111111111111111111111111111111111"
	missingMedia = "media_" + "2222222222222222222222222222222222222222222222222222222222222222"
	removedRoute = "media_" + "3333333333333333333333333333333333333333333333333333333333333333"
)

func routingFixture(t *testing.T) (homeFixture, *routingCatalog, *routingStatuses, http.Handler) {
	t.Helper()
	f := newHomeFixture(t)
	catalog := &routingCatalog{objects: map[string]store.Object{
		routedMedia:  {ID: routedMedia, State: media.ObjectActive},
		missingMedia: {ID: missingMedia, State: media.ObjectActive},
		removedRoute: {ID: removedRoute, State: media.ObjectRemoved},
	}}
	statuses := &routingStatuses{
		healthy:   map[string]bool{f.node2: true, enrollmentID: true},
		available: map[string]map[string]bool{f.node2: {routedMedia: true, removedRoute: true}, enrollmentID: {missingMedia: true}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/routing/v1/media/{media_id}", mediaRoutingHandler(catalog, f.repo, f.repo, statuses, nil))
	if response, _ := homeCall(t, f.h, "PUT", f.alice, `{"node_id":"`+f.node2+`"}`); response.Code != http.StatusOK {
		t.Fatalf("set home: %d", response.Code)
	}
	return f, catalog, statuses, mux
}

func route(t *testing.T, h http.Handler, token, mediaID string) (int, map[string]any, string) {
	t.Helper()
	headers := []string{}
	if token != "" {
		headers = append(headers, "Authorization", "Bearer "+token)
	}
	response, body := call(t, h, "GET", "/api/routing/v1/media/"+mediaID, "", headers...)
	return response.Code, body, response.Body.String()
}

func TestRoutingDecisionAcceptanceCases(t *testing.T) {
	f, _, statuses, h := routingFixture(t)

	// Local available media routes toward the home node.
	code, body, raw := route(t, h, f.alice, routedMedia)
	if code != http.StatusOK || body["target"] != "HOME_NODE" || body["node_id"] != f.node2 {
		t.Fatalf("available: %d %s", code, raw)
	}
	keys := []string{}
	for key := range body {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "node_id,target" {
		t.Fatalf("home decision not minimal: %s", raw)
	}

	// Missing media routes toward Core, even though another Node the user
	// belongs to (node 1) has it: only the home node is considered.
	code, body, raw = route(t, h, f.alice, missingMedia)
	if code != http.StatusOK || body["target"] != "CORE" || len(body) != 1 {
		t.Fatalf("missing: %d %s", code, raw)
	}
	for _, called := range statuses.calls {
		if called != f.node2 {
			t.Fatalf("routing inspected non-home Node %s", called)
		}
	}

	// Offline home node routes toward Core.
	statuses.healthy[f.node2] = false
	code, body, raw = route(t, h, f.alice, routedMedia)
	if code != http.StatusOK || body["target"] != "CORE" || len(body) != 1 || strings.Contains(raw, f.node2) {
		t.Fatalf("offline: %d %s", code, raw)
	}
}

func TestRoutingWithoutHomeNodeOrUsableMedia(t *testing.T) {
	f, _, statuses, h := routingFixture(t)
	before := len(statuses.calls)
	// Bob has no home node: Core, without any Node status lookup.
	if code, body, raw := route(t, h, f.bob, routedMedia); code != http.StatusOK || body["target"] != "CORE" || len(body) != 1 {
		t.Fatalf("no home: %d %s", code, raw)
	}
	if len(statuses.calls) != before {
		t.Fatal("status looked up without a home node")
	}
	unknown := "media_" + strings.Repeat("9", 64)
	for _, id := range []string{unknown, removedRoute} {
		if code, body, raw := route(t, h, f.alice, id); code != http.StatusNotFound || body["error"] != "media unavailable" {
			t.Fatalf("%s: %d %s", id, code, raw)
		}
	}
	statuses.err = errors.New("database password must not leak")
	if code, _, raw := route(t, h, f.alice, routedMedia); code != http.StatusServiceUnavailable || strings.Contains(raw, "password") {
		t.Fatalf("status error: %d %s", code, raw)
	}
}

func TestRoutingRequiresUserSessionBeforeCatalogAccess(t *testing.T) {
	f, catalog, _, h := routingFixture(t)
	for name, token := range map[string]string{"missing": "", "invalid": "not-a-token"} {
		if code, _, _ := route(t, h, token, routedMedia); code != http.StatusUnauthorized {
			t.Fatalf("%s session: %d", name, code)
		}
	}
	if catalog.reads != 0 {
		t.Fatal("unauthenticated request read the catalog")
	}
	for _, path := range []string{"bad-id", routedMedia + "?x=1"} {
		response, _ := call(t, h, "GET", "/api/routing/v1/media/"+path, "", "Authorization", "Bearer "+f.alice)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", path, response.Code)
		}
	}
}
