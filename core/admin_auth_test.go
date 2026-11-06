package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/admins"
)

// The shared development bearer is gone (ARKSEC-005, D7). Its settings are
// refused, not ignored, so a stale deployment cannot believe it is protected
// by a bearer that no longer exists. The value is never echoed.
func TestRemovedDevelopmentAdminSettingsAreRefused(t *testing.T) {
	secret := strings.Repeat("a", 64)
	for _, key := range []string{"ARKNET_CORE_DEV_ADMIN_ENABLED", "ARKNET_CORE_DEV_ADMIN_TOKEN"} {
		for _, value := range []string{"true", "false", secret} {
			_, err := loadConfig(func(k string) string {
				if k == key {
					return value
				}
				return ""
			})
			if err == nil || !strings.Contains(err.Error(), "admin create-token") || strings.Contains(err.Error(), secret) {
				t.Fatalf("%s=%q: err = %v", key, value, err)
			}
		}
	}
	if _, err := loadConfig(func(string) string { return "" }); err != nil {
		t.Fatalf("default config: %v", err)
	}
}

type recordingAdminRepo struct {
	digests   [][32]byte
	principal admins.Principal
	err       error
	audit     []admins.AuditEntry
}

func (r *recordingAdminRepo) Authenticate(_ context.Context, digest [32]byte) (admins.Principal, error) {
	r.digests = append(r.digests, digest)
	return r.principal, r.err
}
func (r *recordingAdminRepo) CreateToken(context.Context, string, []admins.Permission, time.Duration, [32]byte) (admins.Token, error) {
	return admins.Token{}, errors.New("unused")
}
func (r *recordingAdminRepo) RevokeToken(context.Context, string) error { return errors.New("unused") }
func (r *recordingAdminRepo) AppendAudit(_ context.Context, e admins.AuditEntry) error {
	r.audit = append(r.audit, e)
	return nil
}

func TestTokenAuthorizerLooksUpOnlyCanonicalBearerTokensByDigest(t *testing.T) {
	token, digest, _ := admins.NewToken()
	for _, header := range [][]string{nil, {""}, {"Bearer"}, {"Bearer " + token, "Bearer " + token}, {"Basic " + token},
		{"Bearer " + strings.Repeat("a", 64)}, {"Bearer " + token + " "}, {"Bearer " + strings.TrimPrefix(token, admins.TokenPrefix)}} {
		repo := &recordingAdminRepo{}
		request := httptest.NewRequest("GET", "/api/admin/v1/nodes/pending", nil)
		for _, value := range header {
			request.Header.Add("Authorization", value)
		}
		if _, err := newTokenAuthorizer(repo).Authenticate(request); !errors.Is(err, admins.ErrNotFound) || len(repo.digests) != 0 {
			t.Fatalf("%q: err %v, lookups %d", header, err, len(repo.digests))
		}
	}
	repo := &recordingAdminRepo{principal: admins.Principal{TokenID: "t"}}
	request := httptest.NewRequest("GET", "/api/admin/v1/nodes/pending", nil)
	request.Header.Set("Authorization", "bearer "+token)
	if p, err := newTokenAuthorizer(repo).Authenticate(request); err != nil || p.TokenID != "t" || len(repo.digests) != 1 || repo.digests[0] != digest {
		t.Fatalf("valid token: %v %v", p, err)
	}
	// Store failures are not authentication failures: they surface as 503.
	repo = &recordingAdminRepo{err: errors.New("db down")}
	h := withAdministration(newHandler(healthyDatabase{}), &spyRepositories{}, newTokenAuthorizer(repo))
	request = httptest.NewRequest("GET", "/api/admin/v1/nodes/pending", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 503 {
		t.Fatalf("store failure status %d", response.Code)
	}
}
