package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/admins"
)

type memoryAdminStore struct {
	created []struct {
		name   string
		perms  []admins.Permission
		ttl    time.Duration
		digest [32]byte
	}
	revoked []string
}

func (m *memoryAdminStore) Authenticate(context.Context, [32]byte) (admins.Principal, error) {
	return admins.Principal{}, admins.ErrNotFound
}
func (m *memoryAdminStore) CreateToken(_ context.Context, name string, perms []admins.Permission, ttl time.Duration, digest [32]byte) (admins.Token, error) {
	m.created = append(m.created, struct {
		name   string
		perms  []admins.Permission
		ttl    time.Duration
		digest [32]byte
	}{name, perms, ttl, digest})
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	return admins.Token{TokenID: "admtok_" + strings.Repeat("1", 64), AdminID: "admin_" + strings.Repeat("2", 64), AdminName: name,
		Permissions: perms, CreatedAt: now, ExpiresAt: now.Add(ttl)}, nil
}
func (m *memoryAdminStore) RevokeToken(_ context.Context, id string) error {
	if id != "admtok_"+strings.Repeat("1", 64) {
		return admins.ErrNotFound
	}
	m.revoked = append(m.revoked, id)
	return nil
}
func (m *memoryAdminStore) AppendAudit(context.Context, admins.AuditEntry) error { return nil }

func runCLI(store admins.Repository, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := runAdminCommand(context.Background(), args, store, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAdminCLIMintsAScopedTokenOnceAndStoresOnlyItsDigest(t *testing.T) {
	store := &memoryAdminStore{}
	code, out, errOut := runCLI(store, "create-token", "--name", "e2e-run", "--permission", "users.admin", "--permission", "nodes.decide", "--ttl", "2h")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
	var result struct {
		Token       string   `json:"token"`
		TokenID     string   `json:"token_id"`
		AdminID     string   `json:"admin_id"`
		Admin       string   `json:"admin"`
		Permissions []string `json:"permissions"`
		ExpiresAt   string   `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	digest, ok := admins.ParseToken(result.Token)
	if !ok || len(store.created) != 1 || store.created[0].digest != digest || store.created[0].ttl != 2*time.Hour ||
		store.created[0].name != "e2e-run" || strings.Join(result.Permissions, ",") != "nodes.decide,users.admin" ||
		result.ExpiresAt != "2026-11-05T02:00:00Z" || result.Admin != "e2e-run" {
		t.Fatalf("result %+v created %+v", result, store.created)
	}
	if code, out, _ := runCLI(store, "create-token", "--name", "ops", "--permission", "relays.read"); code != 0 ||
		store.created[1].ttl != admins.DefaultTTL || !strings.Contains(out, admins.TokenPrefix) {
		t.Fatalf("default ttl: %d %v", code, store.created[1].ttl)
	}
	if code, _, _ := runCLI(store, "create-token", "--name", "ops", "--permission", "media.admin", "--ttl", "7d"); code != 0 || store.created[2].ttl != 7*24*time.Hour {
		t.Fatal("day TTL not accepted")
	}
}

func TestAdminCLIRefusesBadPolicyWithoutMinting(t *testing.T) {
	for _, args := range [][]string{
		{"create-token", "--name", "ops"},
		{"create-token", "--name", "ops", "--permission", "root"},
		{"create-token", "--name", "ops", "--permission", "users.admin", "--permission", "users.admin"},
		{"create-token", "--name", "Ops", "--permission", "users.admin"},
		{"create-token", "--permission", "users.admin"},
		{"create-token", "--name", "ops", "--permission", "users.admin", "--ttl", "91d"},
		{"create-token", "--name", "ops", "--permission", "users.admin", "--ttl", "-1h"},
		{"create-token", "--name", "ops", "--permission", "users.admin", "extra"},
		{"bogus"}, {},
	} {
		store := &memoryAdminStore{}
		code, out, errOut := runCLI(store, args...)
		if code != 2 || out != "" || len(store.created) != 0 || errOut == "" {
			t.Fatalf("%q: exit %d out %q created %d", args, code, out, len(store.created))
		}
	}
}

type failedAdminOutput struct{}

func (failedAdminOutput) Write([]byte) (int, error) { return 0, context.Canceled }

func TestAdminCLIOutputFailureIsNotSuccess(t *testing.T) {
	for _, command := range [][]string{
		{"create-token", "--name", "ops", "--permission", "nodes.decide"},
		{"revoke-token", "--token-id", "admtok_" + strings.Repeat("1", 64)},
	} {
		store := &memoryAdminStore{}
		var stderr bytes.Buffer
		code := runAdminCommand(context.Background(), command, store, failedAdminOutput{}, &stderr)
		if code != 1 || len(store.revoked) != 1 || !strings.Contains(stderr.String(), "output failed") {
			t.Fatalf("%s: exit=%d revoked=%d stderr=%q", command[0], code, len(store.revoked), stderr.String())
		}
		if strings.Contains(stderr.String(), admins.TokenPrefix) {
			t.Fatal("diagnostic leaked a token")
		}
	}
}

func TestAdminCLIRevokesByTokenID(t *testing.T) {
	store := &memoryAdminStore{}
	id := "admtok_" + strings.Repeat("1", 64)
	if code, out, _ := runCLI(store, "revoke-token", "--token-id", id); code != 0 || len(store.revoked) != 1 || !strings.Contains(out, id) {
		t.Fatalf("revoke: %d %q", code, out)
	}
	if code, _, errOut := runCLI(store, "revoke-token", "--token-id", "admtok_"+strings.Repeat("9", 64)); code != 1 || !strings.Contains(errOut, "not found") {
		t.Fatalf("unknown: %d %q", code, errOut)
	}
	if code, _, _ := runCLI(store, "revoke-token", "--token-id", "nope"); code != 2 {
		t.Fatalf("malformed: %d", code)
	}
}
