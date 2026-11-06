package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/media"
	"arknet/core/internal/nodes"
	"arknet/core/internal/store"
)

const (
	activeMediaID  = "media_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	removedMediaID = "media_abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
)

type mediaRepository struct {
	node           nodes.Node
	key            []byte
	objects        []store.Object
	listReads      int
	getReads       int
	includeRemoved bool
}

func (r *mediaRepository) Enroll(context.Context, string, string) (nodes.Node, error) {
	return r.node, nil
}
func (r *mediaRepository) RegisterEnrollment(context.Context, string, string, []byte) (nodes.Node, error) {
	return r.node, nil
}
func (r *mediaRepository) EnrollmentIdentity(context.Context, string) (nodes.Node, []byte, error) {
	return r.node, r.key, nil
}
func (r *mediaRepository) CreateMediaObject(_ context.Context, object store.Object) (store.Object, error) {
	r.objects = append(r.objects, object)
	return object, nil
}
func (r *mediaRepository) GetMediaObjectByPublicID(_ context.Context, id string) (store.Object, error) {
	r.getReads++
	for _, object := range r.objects {
		if object.ID == id {
			return object, nil
		}
	}
	return store.Object{}, store.ErrNotFound
}
func (r *mediaRepository) ListMediaObjects(_ context.Context, includeRemoved bool, limit int) ([]store.Object, error) {
	r.listReads++
	r.includeRemoved = includeRemoved
	if limit < 1 || limit > 100 {
		return nil, media.ErrInvalidLimit
	}
	return append([]store.Object(nil), r.objects...), nil
}
func (r *mediaRepository) UpdateMediaObjectState(_ context.Context, id string, state store.ObjectState) (store.Object, error) {
	for i := range r.objects {
		if r.objects[i].ID != id {
			continue
		}
		if state != media.ObjectRemoved || r.objects[i].State != media.ObjectActive {
			return store.Object{}, media.ErrInvalidState
		}
		r.objects[i].State = media.ObjectRemoved
		r.objects[i].UpdatedAt = time.Now().UTC()
		return r.objects[i], nil
	}
	return store.Object{}, store.ErrNotFound
}

func approvedMediaRepository(t *testing.T) (*mediaRepository, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 4, 0, 0, 0, time.UTC)
	return &mediaRepository{
		node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted, Capabilities: nodes.AllCapabilities()},
		key:  publicKey,
		objects: []store.Object{
			{ID: activeMediaID, ObjectType: media.ObjectTypeVideo, Title: "Active movie", Size: 1024, State: media.ObjectActive, CreatedAt: now, UpdatedAt: now},
			{ID: removedMediaID, ObjectType: media.ObjectTypeAudio, Title: "Removed album", Size: 2048, State: media.ObjectRemoved, InfoHashV2: strings.Repeat("a", 64), CreatedAt: now, UpdatedAt: now.Add(time.Minute)},
		},
	}, privateKey
}

func TestApprovedNodeCanRetrieveCatalogIncludingRemovedObjects(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	handler := newHandler(nil, repository)
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media"
	request := signedRequest(http.MethodGet, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var result struct {
		Objects []media.APIObject `json:"objects"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Objects) != 2 || result.Objects[0].ID != activeMediaID || result.Objects[1].ID != removedMediaID {
		t.Fatalf("objects = %+v", result.Objects)
	}
	if result.Objects[1].State != string(media.ObjectRemoved) || result.Objects[1].InfoHashV2 != strings.Repeat("a", 64) {
		t.Fatalf("removed object lost tombstone metadata: %+v", result.Objects[1])
	}
	if !repository.includeRemoved {
		t.Fatal("node catalog omitted removed objects")
	}
}

func TestApprovedNodeCanRetrieveRemovedObjectByOpaqueID(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	handler := newHandler(nil, repository)
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + removedMediaID
	request := signedRequest(http.MethodGet, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var object media.APIObject
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	if object.ID != removedMediaID || object.State != string(media.ObjectRemoved) {
		t.Fatalf("object = %+v", object)
	}
}

func TestCatalogRejectsUnauthorizedAndUnapprovedNodesBeforeReading(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(*mediaRepository, *http.Request)
		wantStatus int
	}{
		{"unsigned", func(_ *mediaRepository, request *http.Request) { request.Header.Del("X-ArkNet-Node-Signature") }, http.StatusUnauthorized},
		{"unapproved", func(repository *mediaRepository, _ *http.Request) {
			repository.node.EnrollmentStatus = nodes.EnrollmentPending
			repository.node.TrustStatus = nodes.TrustUntrusted
		}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, privateKey := approvedMediaRepository(t)
			path := "/api/federation/v1/nodes/" + enrollmentID + "/media"
			request := signedRequest(http.MethodGet, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
			test.mutate(repository, request)
			response := httptest.NewRecorder()

			newHandler(nil, repository).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if repository.listReads != 0 {
				t.Fatal("unauthorized request read media catalog")
			}
		})
	}
}

func TestCatalogRejectsMalformedMediaIDBeforeReading(t *testing.T) {
	repository, privateKey := approvedMediaRepository(t)
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/not-an-opaque-id"
	request := signedRequest(http.MethodGet, path, "", enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
	response := httptest.NewRecorder()
	newHandler(nil, repository).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || repository.getReads != 0 {
		t.Fatalf("status = %d, reads = %d, body = %s", response.Code, repository.getReads, response.Body)
	}
}

func TestCatalogDoesNotExposeUnauthenticatedGlobalOrMutationRoutes(t *testing.T) {
	repository, _ := approvedMediaRepository(t)
	handler := newHandler(nil, repository)
	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/api/federation/v1/media", http.StatusNotFound},
		{http.MethodPost, "/api/federation/v1/nodes/" + enrollmentID + "/media", http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Fatalf("%s %s: status = %d", test.method, test.path, response.Code)
		}
	}
}
