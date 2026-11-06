package admins

import (
	"strings"
	"testing"
	"time"
)

func TestPermissionsAreAClosedSet(t *testing.T) {
	for _, value := range []string{"nodes.decide", "users.admin", "media.admin", "relays.read"} {
		if p, ok := ParsePermission(value); !ok || string(p) != value {
			t.Fatalf("permission %q rejected", value)
		}
	}
	for _, value := range []string{"", "*", "admin", "NODES.DECIDE", "nodes.decide ", "memberships", "swarm"} {
		if _, ok := ParsePermission(value); ok {
			t.Fatalf("permission %q accepted", value)
		}
	}
	if len(AllPermissions()) != 4 {
		t.Fatalf("permissions = %v", AllPermissions())
	}
}

func TestTokenIsHighEntropyCanonicalAndOnlyDigestIsKept(t *testing.T) {
	secret, digest, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) || len(secret) != len(TokenPrefix)+43 {
		t.Fatalf("token shape %d", len(secret))
	}
	parsed, ok := ParseToken(secret)
	if !ok || parsed != digest {
		t.Fatal("token does not parse to its digest")
	}
	other, _, _ := NewToken()
	if other == secret {
		t.Fatal("tokens repeat")
	}
	for _, bad := range []string{"", secret[:len(secret)-1], secret + "A", strings.TrimPrefix(secret, TokenPrefix),
		strings.Replace(secret, "_adm_", "_ADM_", 1), secret[:len(secret)-1] + "=", " " + secret,
		// Non-canonical: last base64url character carries non-zero padding bits.
		secret[:len(secret)-1] + nonCanonicalLast(secret[len(secret)-1])} {
		if _, ok := ParseToken(bad); ok {
			t.Fatalf("accepted malformed token %q", bad)
		}
	}
}

func nonCanonicalLast(c byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alphabet, c)
	return string(alphabet[i^1])
}

func TestNamesAndIDs(t *testing.T) {
	for _, name := range []string{"ops", "alice.ops", "e2e-run-1", "a_b"} {
		if !ValidName(name) {
			t.Fatalf("name %q rejected", name)
		}
	}
	for _, name := range []string{"", "ab", "Alice", "-ops", "ops!", strings.Repeat("a", 65), "user_" + strings.Repeat("a", 64)} {
		if ValidName(name) {
			t.Fatalf("name %q accepted", name)
		}
	}
	id, err := NewTokenID()
	if err != nil || !ValidTokenID(id) || !strings.HasPrefix(id, "admtok_") {
		t.Fatalf("token id %q", id)
	}
	admin, err := NewAdminID()
	if err != nil || !ValidAdminID(admin) || ValidTokenID(admin) {
		t.Fatalf("admin id %q", admin)
	}
}

func TestTTLIsBoundedWithThirtyDayDefault(t *testing.T) {
	if DefaultTTL != 30*24*time.Hour || MaxTTL != 90*24*time.Hour {
		t.Fatal("unexpected TTL bounds")
	}
	for _, ttl := range []time.Duration{time.Minute, DefaultTTL, MaxTTL} {
		if !ValidTTL(ttl) {
			t.Fatalf("ttl %s rejected", ttl)
		}
	}
	for _, ttl := range []time.Duration{0, -time.Hour, time.Second, MaxTTL + time.Second} {
		if ValidTTL(ttl) {
			t.Fatalf("ttl %s accepted", ttl)
		}
	}
}

func TestPrincipalHasOnlyGrantedPermissions(t *testing.T) {
	p := Principal{Permissions: []Permission{UsersAdmin}}
	if !p.Has(UsersAdmin) || p.Has(NodesDecide) || p.Has("") {
		t.Fatal("permission check wrong")
	}
}

func TestNormalizePermissionsRejectsUnknownEmptyAndDuplicates(t *testing.T) {
	got, err := NormalizePermissions([]string{"users.admin", "nodes.decide"})
	if err != nil || len(got) != 2 || got[0] != NodesDecide || got[1] != UsersAdmin {
		t.Fatalf("normalized = %v, %v", got, err)
	}
	for _, bad := range [][]string{nil, {}, {"users.admin", "users.admin"}, {"root"}} {
		if _, err := NormalizePermissions(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
