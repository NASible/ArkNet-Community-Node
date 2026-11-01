package globalauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testNode   = "node_" + strings.Repeat("a", 64)
	otherNode  = "node_" + strings.Repeat("c", 64)
	testMember = "member_" + strings.Repeat("b", 64)
	t0         = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
)

type testAssertion struct {
	version, purpose, role, permissions byte
	node, member                        string
	nonce, challenge                    [32]byte
	issued, expires, offline            time.Time
}

func defaultAssertion() testAssertion {
	var nonce [32]byte
	_, _ = rand.Read(nonce[:])
	return testAssertion{version: 1, purpose: 1, role: 1, permissions: 1, node: testNode, member: testMember,
		nonce: nonce, challenge: sha256.Sum256([]byte("verifier")), issued: t0, expires: t0.Add(AssertionLifetime),
		offline: t0.Add(30 * 24 * time.Hour)}
}

// sign mirrors Core's signer (core/internal/assertions) byte for byte.
func (a testAssertion) sign(key ed25519.PrivateKey) string {
	payload := make([]byte, 180)
	payload[0], payload[1] = a.version, a.purpose
	id := KeyIDFor(key.Public().(ed25519.PublicKey))
	copy(payload[2:], id[:])
	node, _ := hex.DecodeString(strings.TrimPrefix(a.node, "node_"))
	member, _ := hex.DecodeString(strings.TrimPrefix(a.member, "member_"))
	copy(payload[10:], node)
	copy(payload[42:], member)
	copy(payload[74:], a.nonce[:])
	copy(payload[106:], a.challenge[:])
	_, _ = rand.Read(payload[138:154])
	binary.BigEndian.PutUint64(payload[154:], uint64(a.issued.Unix()))
	binary.BigEndian.PutUint64(payload[162:], uint64(a.expires.Unix()))
	payload[170], payload[171] = a.role, a.permissions
	binary.BigEndian.PutUint64(payload[172:], uint64(a.offline.Unix()))
	signature := ed25519.Sign(key, append([]byte(AssertionDomain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func newTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func keySet(keys ...ed25519.PrivateKey) []VerificationKey {
	var set []VerificationKey
	for _, key := range keys {
		public := key.Public().(ed25519.PublicKey)
		set = append(set, VerificationKey{ID: KeyIDFor(public), PublicKey: public})
	}
	return set
}

func expectation(a testAssertion) Expectation {
	return Expectation{NodeID: testNode, Nonce: a.nonce, Challenge: a.challenge}
}

func TestVerifyAssertionBindsNodeMemberRoleAndOfflineWindow(t *testing.T) {
	key := newTestKey(t)
	a := defaultAssertion()
	a.role = 2
	claims, err := VerifyAssertion(a.sign(key), keySet(key), expectation(a), t0)
	if err != nil {
		t.Fatal(err)
	}
	if claims.NodeID != testNode || claims.MemberID != testMember || claims.Role != "ADMIN" ||
		!claims.OfflineUntil.Equal(a.offline) || !claims.IssuedAt.Equal(t0) || claims.AssertionID == ([16]byte{}) {
		t.Fatalf("claims: %+v", claims)
	}
}

func TestVerifyAssertionRejectsWrongNodeNonceChallengeAndPurpose(t *testing.T) {
	key := newTestKey(t)
	a := defaultAssertion()
	token := a.sign(key)
	wrong := expectation(a)
	wrong.NodeID = otherNode
	if _, err := VerifyAssertion(token, keySet(key), wrong, t0); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("other Node accepted: %v", err)
	}
	wrong = expectation(a)
	wrong.Nonce[0] ^= 1
	if _, err := VerifyAssertion(token, keySet(key), wrong, t0); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("other nonce accepted: %v", err)
	}
	wrong = expectation(a)
	wrong.Challenge[0] ^= 1
	if _, err := VerifyAssertion(token, keySet(key), wrong, t0); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("other challenge accepted: %v", err)
	}
	for name, mutate := range map[string]func(*testAssertion){
		"purpose":          func(a *testAssertion) { a.purpose = 2 },
		"version":          func(a *testAssertion) { a.version = 2 },
		"role":             func(a *testAssertion) { a.role = 3 },
		"permissions":      func(a *testAssertion) { a.permissions = 3 },
		"lifetime":         func(a *testAssertion) { a.expires = a.issued.Add(AssertionLifetime + time.Second) },
		"offline past D3":  func(a *testAssertion) { a.offline = a.issued.Add(30*24*time.Hour + time.Second) },
		"offline at issue": func(a *testAssertion) { a.offline = a.issued },
	} {
		changed := a
		mutate(&changed)
		if _, err := VerifyAssertion(changed.sign(key), keySet(key), expectation(changed), t0); !errors.Is(err, ErrInvalidAssertion) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}

func TestVerifyAssertionExpiryAndSkewAreExact(t *testing.T) {
	key := newTestKey(t)
	a := defaultAssertion()
	token := a.sign(key)
	last := a.expires.Add(AssertionClockSkew).Add(-time.Second)
	if _, err := VerifyAssertion(token, keySet(key), expectation(a), last); err != nil {
		t.Fatalf("last valid second rejected: %v", err)
	}
	if _, err := VerifyAssertion(token, keySet(key), expectation(a), last.Add(time.Second)); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatal("expired assertion accepted")
	}
	if _, err := VerifyAssertion(token, keySet(key), expectation(a), t0.Add(-AssertionClockSkew)); err != nil {
		t.Fatalf("issued at now+skew rejected: %v", err)
	}
	if _, err := VerifyAssertion(token, keySet(key), expectation(a), t0.Add(-AssertionClockSkew-time.Second)); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatal("future assertion accepted")
	}
}

func TestVerifyAssertionRequiresAPublishedKeyAndCanonicalEncoding(t *testing.T) {
	key, rotated, attacker := newTestKey(t), newTestKey(t), newTestKey(t)
	a := defaultAssertion()
	if _, err := VerifyAssertion(a.sign(rotated), keySet(key, rotated), expectation(a), t0); err != nil {
		t.Fatalf("published second key rejected: %v", err)
	}
	if _, err := VerifyAssertion(a.sign(rotated), keySet(key), expectation(a), t0); !errors.Is(err, ErrUnknownAssertionKey) {
		t.Fatalf("unpublished key: %v", err)
	}
	// A forger who reuses a published key ID but signs with another key.
	forged := a.sign(attacker)
	payload, _ := base64.RawURLEncoding.DecodeString(strings.Split(forged, ".")[0])
	id := KeyIDFor(key.Public().(ed25519.PublicKey))
	copy(payload[2:], id[:])
	signature := ed25519.Sign(attacker, append([]byte(AssertionDomain), payload...))
	forged = base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	if _, err := VerifyAssertion(forged, keySet(key), expectation(a), t0); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("forged signature: %v", err)
	}
	token := a.sign(key)
	for _, bad := range []string{"", token + ".x", token + "=", strings.Replace(token, ".", "", 1), " " + token, token[:len(token)-1]} {
		if _, err := VerifyAssertion(bad, keySet(key), expectation(a), t0); err == nil {
			t.Errorf("non-canonical %q accepted", bad[:min(len(bad), 12)])
		}
	}
}

func TestVerifyStoredGrantChecksSignatureAndNodeButNotLoginExpiry(t *testing.T) {
	key := newTestKey(t)
	a := defaultAssertion()
	token := a.sign(key)
	claims, err := VerifyStoredGrant(token, keySet(key), testNode)
	if err != nil || claims.MemberID != testMember {
		t.Fatalf("stored grant: %+v %v", claims, err)
	}
	if _, err := VerifyStoredGrant(token, keySet(key), otherNode); !errors.Is(err, ErrInvalidAssertion) {
		t.Fatalf("stored grant for another Node: %v", err)
	}
	if _, err := VerifyStoredGrant(token, keySet(newTestKey(t)), testNode); err == nil {
		t.Fatal("stored grant accepted after its key was removed")
	}
}

// goldenAssertion was issued by Core's signer (core/internal/assertions) with
// the seed 00 01 .. 1f, at 2026-10-30T12:00:00Z, role ADMIN, nonce 0x11..,
// challenge 0x22... Core's TestGoldenAssertionMatchesNodeAgent verifies the
// same string, so the two implementations cannot drift apart.
const goldenAssertion = "AQFWR1qnVGNHTKqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqu7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7sRERERERERERERERERERERERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiIiUiQqHYXUZ3y4wZRHaVgQuwAAAABq5IbAAAAAAGrkhzgCAQAAAABrDBPA.CafrIteZ9LJqJeEuooQxW3gl7q7QwSKm8ZdjtkGk2T6ClJeRYZJppPRF2Tp8OVUfmH-sZorUtejj7XIkT8etBw"

func TestCoreIssuedGoldenAssertionVerifies(t *testing.T) {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i)
	}
	var nonce, challenge [32]byte
	for i := range nonce {
		nonce[i], challenge[i] = 0x11, 0x22
	}
	claims, err := VerifyAssertion(goldenAssertion, keySet(ed25519.NewKeyFromSeed(seed)),
		Expectation{NodeID: testNode, Nonce: nonce, Challenge: challenge}, t0.Add(time.Minute))
	if err != nil || claims.MemberID != testMember || claims.Role != "ADMIN" || !claims.OfflineUntil.Equal(t0.Add(MaxOfflineWindow)) {
		t.Fatalf("golden: %+v %v", claims, err)
	}
}
