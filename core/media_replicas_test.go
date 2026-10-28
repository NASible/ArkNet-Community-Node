package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/store"
)

type replicaRepository struct {
	*mediaRepository
	reports  int
	replica  store.MediaReplica
	reportID string
	size     int64
	hash     string
	err      error
}

func (r *replicaRepository) ReportMediaReplicaAvailable(_ context.Context, nodeID, mediaID string, size int64, hash string) (store.MediaReplica, error) {
	r.reports++
	r.reportID = mediaID
	r.size = size
	r.hash = hash
	if r.err != nil {
		return store.MediaReplica{}, r.err
	}
	if r.replica.AvailableAt.IsZero() {
		r.replica = store.MediaReplica{NodeID: nodeID, MediaID: mediaID, AvailableAt: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	}
	return r.replica, nil
}

func (r *replicaRepository) GetMediaReplica(context.Context, string, string) (store.MediaReplica, error) {
	if r.replica.MediaID == "" {
		return store.MediaReplica{}, store.ErrNotFound
	}
	return r.replica, nil
}

func (r *replicaRepository) ListEligibleMediaSources(context.Context, string, int) ([]store.MediaReplica, error) {
	return nil, nil
}

func TestApprovedNodeCanIdempotentlyReportVerifiedReplica(t *testing.T) {
	catalog, privateKey := approvedMediaRepository(t)
	catalog.objects[0].InfoHashV2 = strings.Repeat("b", 64)
	repository := &replicaRepository{mediaRepository: catalog}
	path := "/api/federation/v1/nodes/" + enrollmentID + "/media/" + activeMediaID + "/replicas"
	body := fmt.Sprintf(`{"size":%d,"info_hash_v2":%q}`, catalog.objects[0].Size, catalog.objects[0].InfoHashV2)

	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		request := signedRequest(http.MethodPut, path, body, enrollmentID, privateKey, fmt.Sprint(time.Now().Unix()))
		newHandler(nil, repository).ServeHTTP(response, request)
		if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
			t.Fatalf("attempt %d: status = %d, body = %s", attempt, response.Code, response.Body.String())
		}
	}
	if repository.reports != 2 || repository.getReads != 2 {
		t.Fatalf("reports = %d, catalog reads = %d", repository.reports, repository.getReads)
	}
	if repository.reportID != activeMediaID || repository.size != 1024 || repository.hash != strings.Repeat("b", 64) {
		t.Fatalf("stored report = id %q size %d hash %q", repository.reportID, repository.size, repository.hash)
	}
}
