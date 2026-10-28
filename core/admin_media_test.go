package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"arknet/core/internal/media"
)

type mediaAdminRepository struct {
	*mediaRepository
	*adminStoreStub
}

func TestAdminCanRemoveMediaWithoutFilesystemDetails(t *testing.T) {
	mediaRepository, _ := approvedMediaRepository(t)
	repository := &mediaAdminRepository{mediaRepository: mediaRepository, adminStoreStub: &adminStoreStub{}}
	token := strings.Repeat("a", 64)
	handler := withAdministration(newHandler(nil, repository), repository, developmentAdminAuthorizer(token))
	request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/"+activeMediaID+"/remove", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || repository.objects[0].State != media.ObjectRemoved {
		t.Fatalf("remove: %d %s state=%s", response.Code, response.Body, repository.objects[0].State)
	}
	for _, forbidden := range []string{"path", "filesystem", "jellyfin", "replica"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("response exposed %q: %s", forbidden, response.Body)
		}
	}

	request = httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/"+activeMediaID+"/remove", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("repeat remove: %d %s", response.Code, response.Body)
	}
}

func TestMediaRemovalAdministrationIsProtectedAndStrict(t *testing.T) {
	mediaRepository, _ := approvedMediaRepository(t)
	repository := &mediaAdminRepository{mediaRepository: mediaRepository, adminStoreStub: &adminStoreStub{}}
	token := strings.Repeat("b", 64)
	handler := withAdministration(newHandler(nil, repository), repository, developmentAdminAuthorizer(token))
	for _, test := range []struct {
		name, id, suffix, body, authorization string
		want                                  int
	}{
		{"unauthorized", activeMediaID, "", "", "", http.StatusUnauthorized},
		{"invalid id", "not-media", "", "", "Bearer " + token, http.StatusBadRequest},
		{"query", activeMediaID, "?x=1", "", "Bearer " + token, http.StatusBadRequest},
		{"body", activeMediaID, "", `{}`, "Bearer " + token, http.StatusBadRequest},
		{"missing", "media_" + strings.Repeat("f", 64), "", "", "Bearer " + token, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/media/"+test.id+"/remove"+test.suffix, strings.NewReader(test.body))
			request.Header.Set("Authorization", test.authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
		})
	}
}
