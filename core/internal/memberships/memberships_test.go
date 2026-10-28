package memberships

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestPolicyBounds(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	if err := ValidatePolicy(RoleMember, now.Add(time.Hour), 1, now); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePolicy(RoleAdmin, now.Add(MaxInvitationLifetime), MaxInvitationUses, now); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		role    Role
		expires time.Time
		uses    int
		want    error
	}{
		"unknown role":   {"OWNER", now.Add(time.Hour), 1, ErrInvalidRole},
		"lowercase role": {"member", now.Add(time.Hour), 1, ErrInvalidRole},
		"past":           {RoleMember, now.Add(-time.Second), 1, ErrInvalidExpiration},
		"too soon":       {RoleMember, now.Add(MinInvitationLifetime - time.Second), 1, ErrInvalidExpiration},
		"too far":        {RoleMember, now.Add(MaxInvitationLifetime + time.Second), 1, ErrInvalidExpiration},
		"zero uses":      {RoleMember, now.Add(time.Hour), 0, ErrInvalidMaxUses},
		"too many uses":  {RoleMember, now.Add(time.Hour), MaxInvitationUses + 1, ErrInvalidMaxUses},
		"negative uses":  {RoleMember, now.Add(time.Hour), -1, ErrInvalidMaxUses},
	} {
		if err := ValidatePolicy(test.role, test.expires, test.uses, now); err != test.want {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestSecretsAreUnguessableAndOnlyDigestIsKept(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		secret, digest, err := NewSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) != 43 || seen[secret] || digest != sha256.Sum256([]byte(secret)) {
			t.Fatalf("secret %q", secret)
		}
		seen[secret] = true
		if got, ok := SecretDigest(secret); !ok || got != digest {
			t.Fatal("valid secret rejected")
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("A", 42) + "=", strings.Repeat("+", 43)} {
		if _, ok := SecretDigest(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestMemberIDsAreRandomPairwiseValues(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 256; i++ {
		id, err := NewMemberID()
		if err != nil || !ValidMemberID(id) || seen[id] {
			t.Fatalf("%q %v", id, err)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "member_", "user_" + strings.Repeat("a", 64), "member_" + strings.Repeat("A", 64), "member_" + strings.Repeat("a", 63), "member_" + strings.Repeat("a", 65)} {
		if ValidMemberID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestInvitationIDs(t *testing.T) {
	id, err := NewInvitationID()
	if err != nil || !ValidInvitationID(id) {
		t.Fatalf("%q %v", id, err)
	}
	for _, bad := range []string{"", "node_" + id[7:], id + "0", strings.ToUpper(id)} {
		if ValidInvitationID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}
