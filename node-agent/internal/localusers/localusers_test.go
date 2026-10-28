package localusers

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
)

func TestLocalIDsAreRandomAndNeverGlobalShaped(t *testing.T) {
	first, err := NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewUserID()
	if !ValidUserID(first) || first == second {
		t.Fatalf("%q %q", first, second)
	}
	for _, bad := range []string{"", "user_" + strings.Repeat("a", 64), "member_" + strings.Repeat("a", 64), first + "0", strings.ToUpper(first)} {
		if ValidUserID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestUsernameRules(t *testing.T) {
	if got, err := NormalizeUsername("Alice"); err != nil || got != "alice" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"ab", ".alice", "al ice", "alice@x", strings.Repeat("a", 33)} {
		if _, err := NormalizeUsername(bad); err != ErrInvalidUsername {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestPasswordHashing(t *testing.T) {
	ctx := context.Background()
	const password = "correct horse battery staple"
	hash, err := HashPassword(ctx, password)
	if err != nil || !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") || strings.Contains(hash, password) {
		t.Fatalf("%q %v", hash, err)
	}
	if ok, err := VerifyPassword(ctx, hash, password); !ok || err != nil {
		t.Fatal("valid password rejected")
	}
	if ok, _ := VerifyPassword(ctx, hash, password+"x"); ok {
		t.Fatal("wrong password accepted")
	}
	for _, bad := range []string{"", "plaintext", strings.Replace(hash, "m=19456", "m=4194304", 1)} {
		if ok, err := VerifyPassword(ctx, bad, password); ok || err != nil {
			t.Fatalf("malformed hash %q matched", bad)
		}
	}
	if _, err := HashPassword(ctx, "short"); err != ErrInvalidPassword {
		t.Fatal("short password accepted")
	}
}

func TestSessionTokens(t *testing.T) {
	token, digest, err := NewSessionToken()
	if err != nil || len(token) != 43 || digest != sha256.Sum256([]byte(token)) {
		t.Fatalf("%q %v", token, err)
	}
	if got, ok := SessionDigest(token); !ok || got != digest {
		t.Fatal("valid token rejected")
	}
	if _, ok := SessionDigest(token + "A"); ok {
		t.Fatal("non-canonical token accepted")
	}
}
