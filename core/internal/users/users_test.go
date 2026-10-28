package users

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestUserIDsAreOpaqueRandomAndValidated(t *testing.T) {
	first, err := NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	second, _ := NewUserID()
	if !ValidUserID(first) || first == second {
		t.Fatalf("ids %q %q", first, second)
	}
	for _, bad := range []string{"", "user_", "node_" + first[5:], strings.ToUpper(first), first + "0", first[:68] + "g"} {
		if ValidUserID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestUsernameNormalization(t *testing.T) {
	for in, want := range map[string]string{"Alice": "alice", "bob.smith_2": "bob.smith_2", "a-b": "a-b"} {
		if got, err := NormalizeUsername(in); err != nil || got != want {
			t.Fatalf("%q => %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "ab", strings.Repeat("a", 33), ".alice", "-alice", "al ice", "alice@x", "ålice", "al\x00ice"} {
		if _, err := NormalizeUsername(bad); err != ErrInvalidUsername {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestPasswordHashIsSaltedArgon2idAndVerifies(t *testing.T) {
	ctx := context.Background()
	const password = "correct horse battery staple"
	first, err := HashPassword(ctx, password)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := HashPassword(ctx, password)
	if !strings.HasPrefix(first, "$argon2id$v=19$m=19456,t=2,p=1$") || first == second || strings.Contains(first, password) {
		t.Fatalf("hash %q / %q", first, second)
	}
	if ok, err := VerifyPassword(ctx, first, password); !ok || err != nil {
		t.Fatalf("valid password rejected: %v", err)
	}
	if ok, _ := VerifyPassword(ctx, first, password+"x"); ok {
		t.Fatal("wrong password accepted")
	}
}

func TestPasswordLengthBounds(t *testing.T) {
	for _, bad := range []string{"", strings.Repeat("a", MinPasswordBytes-1), strings.Repeat("a", MaxPasswordBytes+1), strings.Repeat("a", 12) + "\xff"} {
		if _, err := HashPassword(context.Background(), bad); err != ErrInvalidPassword {
			t.Fatalf("len %d: %v", len(bad), err)
		}
	}
}

func TestMalformedOrUnboundedStoredHashesNeverMatch(t *testing.T) {
	ctx := context.Background()
	valid, _ := HashPassword(ctx, "correct horse battery staple")
	parts := strings.Split(valid, "$")
	for name, encoded := range map[string]string{
		"empty":        "",
		"plaintext":    "correct horse battery staple",
		"argon2i":      strings.Replace(valid, "argon2id", "argon2i", 1),
		"version":      strings.Replace(valid, "v=19", "v=16", 1),
		"huge memory":  strings.Replace(valid, "m=19456", "m=4194304", 1),
		"huge time":    strings.Replace(valid, "t=2", "t=1000", 1),
		"zero threads": strings.Replace(valid, "p=1", "p=0", 1),
		"param suffix": strings.Replace(valid, "p=1", "p=1x", 1),
		"short salt":   strings.Join([]string{"", parts[1], parts[2], parts[3], "AAAA", parts[5]}, "$"),
		"bad base64":   strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], "!!!"}, "$"),
	} {
		if ok, err := VerifyPassword(ctx, encoded, "correct horse battery staple"); ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v", name, ok, err)
		}
	}
}

func TestHashingRespectsContextWhenSlotsAreBusy(t *testing.T) {
	for i := 0; i < maxConcurrentHashes; i++ {
		hashSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentHashes; i++ {
			<-hashSlots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := HashPassword(ctx, "correct horse battery staple"); err != ErrHashUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionTokensAreRandomAndOnlyDigestIsStored(t *testing.T) {
	token, digest, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	other, _, _ := NewSessionToken()
	if len(token) != 43 || token == other || digest != sha256.Sum256([]byte(token)) {
		t.Fatalf("token %q", token)
	}
	if got, ok := SessionDigest(token); !ok || got != digest {
		t.Fatal("valid token rejected")
	}
	for _, bad := range []string{"", token[:42], token + "A", token[:42] + "=", strings.Replace(token, token[:1], "+", 1)} {
		if _, ok := SessionDigest(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
