package operators

import (
	"crypto/sha256"
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B, SHA-1 secret, truncated to the low 6 of the 8 digits.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471",
		1234567890: "005924", 2000000000: "279037", 20000000000: "353130"} {
		if got := TOTPCode(secret, unix/30); got != want {
			t.Fatalf("T=%d: got %s want %s", unix, got, want)
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret, _ := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	step := TOTPStep(now)
	for _, offset := range []int64{-1, 0, 1} {
		if got, ok := VerifyTOTP(secret, TOTPCode(secret, step+offset), now, 0); !ok || got != step+offset {
			t.Fatalf("offset %d: %d %v", offset, got, ok)
		}
	}
	for _, offset := range []int64{-2, 2} {
		if _, ok := VerifyTOTP(secret, TOTPCode(secret, step+offset), now, 0); ok {
			t.Fatalf("offset %d accepted", offset)
		}
	}
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, step), now, step); ok {
		t.Fatal("replayed step accepted")
	}
	if _, ok := VerifyTOTP(secret, TOTPCode(secret, step-1), now, step); ok {
		t.Fatal("older step accepted after a newer one")
	}
	for _, bad := range []string{"", "12345", "1234567", "12a456", " 12345"} {
		if _, ok := VerifyTOTP(secret, bad, now, 0); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
	if _, ok := VerifyTOTP(secret[:10], TOTPCode(secret[:10], step), now, 0); ok {
		t.Fatal("short secret accepted")
	}
}

func TestSetupCodeFormatAndDigestNormalization(t *testing.T) {
	code, digest, err := NewSetupCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 39 || strings.Count(code, "-") != 7 {
		t.Fatalf("code format %q", code)
	}
	for _, input := range []string{code, strings.ToLower(code), strings.ReplaceAll(code, "-", ""), strings.ReplaceAll(code, "-", " ")} {
		if got, ok := SetupCodeDigest(input); !ok || got != digest {
			t.Fatalf("%q did not normalize", input)
		}
	}
	for _, bad := range []string{"", code[:38], code + "A", strings.Replace(code, code[:1], "1", 1)} {
		if _, ok := SetupCodeDigest(bad); ok && bad != code {
			t.Fatalf("%q accepted", bad)
		}
	}
	other, _, _ := NewSetupCode()
	if other == code {
		t.Fatal("setup codes repeat")
	}
}

func TestRecoveryCodesAreDistinctAndDigestOnly(t *testing.T) {
	codes, digests, err := NewRecoveryCodes()
	if err != nil || len(codes) != RecoveryCodeCount || len(digests) != RecoveryCodeCount {
		t.Fatal(err)
	}
	seen := map[[32]byte]bool{}
	for i, code := range codes {
		if len(code) != 11 || code[5] != '-' {
			t.Fatalf("format %q", code)
		}
		got, ok := RecoveryCodeDigest(strings.ToLower(code))
		if !ok || got != digests[i] || got == sha256.Sum256([]byte(code)) && strings.Contains(code, "-") {
			t.Fatalf("digest %q", code)
		}
		seen[got] = true
	}
	if len(seen) != RecoveryCodeCount {
		t.Fatal("duplicate recovery codes")
	}
}

func TestTOTPURIIsImportable(t *testing.T) {
	secret := []byte("12345678901234567890")
	parsed, err := url.Parse(TOTPURI("alice", secret))
	if err != nil || parsed.Scheme != "otpauth" || parsed.Host != "totp" {
		t.Fatalf("%v %v", parsed, err)
	}
	q := parsed.Query()
	if q.Get("secret") != EncodeTOTPSecret(secret) || q.Get("issuer") != "ArkNet Node" || q.Get("digits") != "6" || q.Get("period") != "30" {
		t.Fatalf("query %v", q)
	}
}

func TestOperatorIDAndFailureDelay(t *testing.T) {
	id, err := NewOperatorID()
	if err != nil || !ValidOperatorID(id) || ValidOperatorID("local_"+id[3:]) || ValidOperatorID(id[:66]) {
		t.Fatalf("%q %v", id, err)
	}
	for failures, want := range map[int]time.Duration{0: 0, 4: 0, 5: time.Second, 6: 2 * time.Second, 14: 512 * time.Second, 15: MaxFailureDelay, 1000: MaxFailureDelay} {
		if got := FailureDelay(failures); got != want {
			t.Fatalf("%d: %v want %v", failures, got, want)
		}
	}
}
