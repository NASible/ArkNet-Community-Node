package main

import (
	"context"
	"encoding/json"
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
	"arknet/node-agent/internal/mediaselection"
)

const selectionID = "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type selectionRepository struct {
	objects  []mediaselection.Object
	queued   []catalog.Object
	queueErr error
	listErr  error
}

func (s *selectionRepository) TransitionMediaSelection(_ context.Context, id string, expected, next mediaselection.State) error {
	for i := range s.objects {
		if s.objects[i].ID == id && s.objects[i].LocalState == expected {
			s.objects[i].LocalState = next
			return nil
		}
	}
	return errors.New("state conflict")
}

func (s *selectionRepository) EnrollmentIdentity(context.Context) (enrollment.Identity, error) {
	return enrollment.Identity{CoreURL: "https://core.example", Status: "APPROVED"}, nil
}
func (s *selectionRepository) ListMediaSelections(context.Context) ([]mediaselection.Object, error) {
	return s.objects, s.listErr
}
func (s *selectionRepository) RetainRemovedMediaSelections(_ context.Context, objects []catalog.Object) error {
	for _, object := range objects {
		for i := range s.objects {
			if object.ID == s.objects[i].ID && object.State == catalog.Removed {
				s.objects[i].State = catalog.Removed
				if s.objects[i].LocalState == mediaselection.Available || s.objects[i].LocalState == mediaselection.RemovedRetained {
					s.objects[i].LocalState = mediaselection.RemovedRetained
				} else {
					s.objects[i].LocalState = mediaselection.NotPresent
				}
			}
		}
	}
	return nil
}
func (s *selectionRepository) QueueMediaSelection(_ context.Context, object catalog.Object, _ int64) (mediaselection.Object, error) {
	s.queued = append(s.queued, object)
	if s.queueErr != nil {
		return mediaselection.Object{}, s.queueErr
	}
	selected := mediaselection.Object{ID: object.ID, Title: object.Title, Size: object.Size, State: object.State, LocalState: mediaselection.Queued}
	s.objects = append(s.objects, selected)
	return selected, nil
}
func catalogTestObject() catalog.Object {
	return catalog.Object{ID: selectionID, Title: "A movie", Size: 4096, ContentType: catalog.Video, State: catalog.Active, CreatedAt: time.Now(), UpdatedAt: time.Now()}
}
func selectionHandler(s *selectionRepository, objects []catalog.Object, fetchErr error) http.Handler {
	return newRoutes(testRepository{}, mediaOpt{repository: s, allocation: 8192, coreURL: "https://core.example",
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) { return objects, fetchErr },
	})
}
func selectionRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}
func TestMediaListThenOperatorQueuesCoreObject(t *testing.T) {
	s := &selectionRepository{}
	h := selectionHandler(s, []catalog.Object{catalogTestObject()}, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("GET", "/api/v1/media", ""))
	var result mediaListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Objects) != 1 || result.Objects[0].LocalState != mediaselection.NotPresent || !result.CoreAvailable || result.AllocationBytes != 8192 || result.ReservedBytes != 0 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("POST", "/api/v1/media/"+selectionID+"/selection", "{}"))
	var selected mediaselection.Object
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &selected) != nil || selected.LocalState != mediaselection.Queued || selected.Size != 4096 || len(s.queued) != 1 {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("GET", "/api/v1/media", ""))
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ReservedBytes != 4096 || result.Objects[0].LocalState != mediaselection.Queued {
		t.Fatalf("saved queue: %s", w.Body)
	}
}

func TestMediaOfflineKeepsLocalSelections(t *testing.T) {
	s := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Title: "Saved", Size: 4096, State: catalog.Active, LocalState: mediaselection.Queued}}}
	h := selectionHandler(s, nil, errors.New("secret Core URL"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("GET", "/api/v1/media", ""))
	var result mediaListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.CoreAvailable || result.ReservedBytes != 4096 || len(result.Objects) != 1 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("offline: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, selectionRequest("POST", "/api/v1/media/"+selectionID+"/selection", "{}"))
	if w.Code != 503 || len(s.queued) != 0 {
		t.Fatalf("offline queue: %d %s", w.Code, w.Body)
	}
}

func TestDeleteRemovedRetainedMediaIsExplicitAndLocal(t *testing.T) {
	mediaDir := t.TempDir()
	path := filepath.Join(mediaDir, selectionID+".mp4")
	if err := os.WriteFile(path, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Title: "Retained", Size: 8, State: catalog.Removed, LocalState: mediaselection.RemovedRetained}}}
	handler := newRoutes(testRepository{}, mediaOpt{repository: repo, mediaDir: mediaDir})
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/media/"+selectionID+"/retained", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", response.Code, response.Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("retained file still exists: %v", err)
	}
	if repo.objects[0].LocalState != mediaselection.NotPresent {
		t.Fatalf("state=%s", repo.objects[0].LocalState)
	}
}

func TestCoreRemovalKeepsAvailableFileUntilOperatorDeletesIt(t *testing.T) {
	mediaDir := t.TempDir()
	path := filepath.Join(mediaDir, selectionID+".mp4")
	if err := os.WriteFile(path, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := &selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Title: "Retained", Size: 8, State: catalog.Active, LocalState: mediaselection.Available}}}
	handler := newRoutes(testRepository{}, mediaOpt{repository: repo, mediaDir: mediaDir, coreURL: "https://core.example", fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
		object := catalogTestObject()
		object.State = catalog.Removed
		return []catalog.Object{object}, nil
	}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/media", nil))
	if response.Code != http.StatusOK || repo.objects[0].LocalState != mediaselection.RemovedRetained {
		t.Fatalf("sync: %d %s state=%s", response.Code, response.Body, repo.objects[0].LocalState)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "retained" {
		t.Fatalf("Core removal changed local file: %q %v", got, err)
	}
}

func TestMediaQueueRejectsUntrustedRequestsBeforeCoreOrStorage(t *testing.T) {
	for _, tt := range []struct {
		name, path, body, contentType, origin, site string
		want                                        int
	}{
		{"forged metadata", selectionID, `{"size":1}`, "application/json", "", "", 400},
		{"null", selectionID, "null", "application/json", "", "", 400},
		{"trailing JSON", selectionID, "{}{}", "application/json", "", "", 400},
		{"path id", "bad-id", "{}", "application/json", "", "", 400},
		{"form", selectionID, "{}", "text/plain", "", "", 400},
		{"cross-origin", selectionID, "{}", "application/json", "https://evil.example", "", 403},
		{"cross-site", selectionID, "{}", "application/json", "", "cross-site", 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &selectionRepository{}
			calls := 0
			h := newRoutes(testRepository{}, mediaOpt{repository: s, allocation: 8192, coreURL: "https://core.example", fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
				calls++
				return []catalog.Object{catalogTestObject()}, nil
			}})
			r := selectionRequest("POST", "/api/v1/media/"+tt.path+"/selection", tt.body)
			r.Header.Set("Content-Type", tt.contentType)
			r.Header.Set("Origin", tt.origin)
			r.Header.Set("Sec-Fetch-Site", tt.site)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want || calls != 0 || len(s.queued) != 0 {
				t.Fatalf("status=%d calls=%d queued=%d", w.Code, calls, len(s.queued))
			}
		})
	}
}

func TestMediaQueueAuthoritativeFailures(t *testing.T) {
	for _, tt := range []struct {
		name     string
		objects  []catalog.Object
		queueErr error
		status   int
		code     string
	}{
		{"missing", nil, nil, 404, "media_not_found"},
		{"removed", []catalog.Object{{ID: selectionID, State: catalog.Removed}}, nil, 409, "media_removed"},
		{"allocation", []catalog.Object{catalogTestObject()}, mediaselection.ErrAllocationExceeded, 409, "storage_allocation_exceeded"},
		{"database", []catalog.Object{catalogTestObject()}, errors.New("secret password"), 503, "local_media_unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &selectionRepository{queueErr: tt.queueErr}
			w := httptest.NewRecorder()
			selectionHandler(s, tt.objects, nil).ServeHTTP(w, selectionRequest("POST", "/api/v1/media/"+selectionID+"/selection", "{}"))
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.code) || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
		})
	}
}
