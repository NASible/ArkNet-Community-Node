package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/mediaselection"
)

type configuredMediaRepository struct {
	testRepository
	selectionRepository
}

func TestConfiguredHandlerWiresLocalMediaAndAllocationWithoutCore(t *testing.T) {
	repository := &configuredMediaRepository{selectionRepository: selectionRepository{objects: []mediaselection.Object{{ID: selectionID, Title: "Saved movie", Size: 4096, State: catalog.Active, LocalState: mediaselection.Queued}}}}
	w := httptest.NewRecorder()
	newConfiguredHandler(repository, config{mediaStorageAllocation: 8192}, nil).ServeHTTP(w, selectionRequest("GET", "/api/v1/media", ""))
	var result mediaListResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.AllocationBytes != 8192 || result.ReservedBytes != 4096 || result.CoreAvailable || len(result.Objects) != 1 {
		t.Fatalf("configured media: %d %s", w.Code, w.Body)
	}
}
