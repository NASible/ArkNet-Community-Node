package assertions

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/memberships"
)

var (
	nodeA   = "node_" + strings.Repeat("a", 64)
	nodeB   = "node_" + strings.Repeat("b", 64)
	member1 = "member_" + strings.Repeat("1", 64)
	member2 = "member_" + strings.Repeat("2", 64)
	t0      = time.Unix(1_800_000_000, 0).UTC()
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newSigner(t *testing.T, key ed25519.PrivateKey) *Signer {
	t.Helper()
	signer, err := NewSigner(key, MaxOfflineWindow)
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return t0 }
	return signer
}

func nonces() (nonce, challenge [32]byte) {
	_, _ = rand.Read(nonce[:])
	_, _ = rand.Read(challenge[:])
	return
}

func keysOf(keys ...ed25519.PrivateKey) map[KeyID]ed25519.PublicKey {
	set := map[KeyID]ed25519.PublicKey{}
	for _, key := range keys {
		public := key.Public().(ed25519.PublicKey)
		set[KeyIDFor(public)] = public
	}
	return set
}

func TestIssuedAssertionBindsNodeMemberPurposeNonceChallengeAndExpiry(t *testing.T) {
	key := newKey(t)
	nonce, challenge := nonces()
	token, issued, err := newSigner(t, key).Issue(nodeA, member1, memberships.RoleAdmin, nonce, challenge)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := Verify(token, keysOf(key), Expectation{NodeID: nodeA, Nonce: nonce, Challenge: challenge}, t0)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims != issued {
		t.Fatalf("verified claims %+v differ from issued %+v", claims, issued)
	}
	if claims.NodeID != nodeA || claims.MemberID != member1 || claims.Role != memberships.RoleAdmin ||
		claims.Purpose != PurposeNodeLogin || claims.Nonce != nonce || claims.Challenge != challenge ||
		claims.Permissions != PermissionMediaAccess {
		t.Fatalf("binding: %+v", claims)
	}
	if !claims.IssuedAt.Equal(t0) || !claims.ExpiresAt.Equal(t0.Add(Lifetime)) || !claims.OfflineUntil.Equal(t0.Add(MaxOfflineWindow)) {
		t.Fatalf("times: %+v", claims)
	}
	if claims.KeyID != KeyIDFor(key.Public().(ed25519.PublicKey)) {
		t.Fatal("key ID does not name the signing key")
	}
	other, _, _ := newSigner(t, key).Issue(nodeA, member1, memberships.RoleAdmin, nonce, challenge)
	if other == token {
		t.Fatal("assertion IDs must be random per issuance")
	}
}

func TestVerifyRejectsAnotherNodesNonceOrChallenge(t *testing.T) {
	key := newKey(t)
	nonce, challenge := nonces()
	token, _, _ := newSigner(t, key).Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	otherNonce, otherChallenge := nonces()
	for name, expect := range map[string]Expectation{
		"wrong node":      {NodeID: nodeB, Nonce: nonce, Challenge: challenge},
		"wrong nonce":     {NodeID: nodeA, Nonce: otherNonce, Challenge: challenge},
		"wrong challenge": {NodeID: nodeA, Nonce: nonce, Challenge: otherChallenge},
	} {
		if _, err := Verify(token, keysOf(key), expect, t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestTamperedFieldsFailSignature(t *testing.T) {
	key := newKey(t)
	nonce, challenge := nonces()
	token, _, _ := newSigner(t, key).Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	payloadPart, signature, _ := strings.Cut(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(payloadPart)
	// Swap in another member ID (bytes 42..74) or flip the role: both must fail.
	other, _ := memberIDBytes(member2)
	for name, mutate := range map[string]func([]byte){
		"member": func(p []byte) { copy(p[offsetMember:offsetMember+32], other[:]) },
		"role":   func(p []byte) { p[offsetRole] = roleByte(memberships.RoleAdmin) },
		"expiry": func(p []byte) { p[offsetExpiresAt+7]++ },
	} {
		changed := append([]byte(nil), payload...)
		mutate(changed)
		forged := base64.RawURLEncoding.EncodeToString(changed) + "." + signature
		if _, err := Verify(forged, keysOf(key), Expectation{NodeID: nodeA, Nonce: nonce, Challenge: challenge}, t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s tampering accepted: %v", name, err)
		}
	}
}

func TestExpiryIsExactAtTheSkewedBoundary(t *testing.T) {
	key := newKey(t)
	nonce, challenge := nonces()
	token, claims, _ := newSigner(t, key).Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	expect := Expectation{NodeID: nodeA, Nonce: nonce, Challenge: challenge}
	last := claims.ExpiresAt.Add(ClockSkew).Add(-time.Second)
	if _, err := Verify(token, keysOf(key), expect, last); err != nil {
		t.Fatalf("one second before the boundary: %v", err)
	}
	if _, err := Verify(token, keysOf(key), expect, claims.ExpiresAt.Add(ClockSkew)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("at the boundary: %v", err)
	}
	if _, err := Verify(token, keysOf(key), expect, t0.Add(-ClockSkew).Add(-time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatal("assertion issued in the future (beyond skew) was accepted")
	}
	if _, err := Verify(token, keysOf(key), expect, t0.Add(-ClockSkew)); err != nil {
		t.Fatalf("issued exactly at now+skew must be accepted: %v", err)
	}
}

func TestKeyRotationPublishesBothAndSignsWithActiveKey(t *testing.T) {
	oldKey, newKeyPair := newKey(t), newKey(t)
	nonce, challenge := nonces()
	expect := Expectation{NodeID: nodeA, Nonce: nonce, Challenge: challenge}
	oldToken, oldClaims, _ := newSigner(t, oldKey).Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)

	// After rotation the new key signs; the old key is still published.
	rotated := newSigner(t, newKeyPair)
	newToken, newClaims, _ := rotated.Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	if newClaims.KeyID == oldClaims.KeyID || newClaims.KeyID != rotated.KeyID() {
		t.Fatal("rotated signer must use the new key ID")
	}
	both := keysOf(oldKey, newKeyPair)
	for name, token := range map[string]string{"old": oldToken, "new": newToken} {
		if _, err := Verify(token, both, expect, t0); err != nil {
			t.Errorf("%s assertion with both keys published: %v", name, err)
		}
	}
	// Once the old key is withdrawn, its assertions fail; a key ID collision
	// with the wrong public key fails the signature.
	if _, err := Verify(oldToken, keysOf(newKeyPair), expect, t0); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("withdrawn key: %v", err)
	}
	impostor := map[KeyID]ed25519.PublicKey{oldClaims.KeyID: newKeyPair.Public().(ed25519.PublicKey)}
	if _, err := Verify(oldToken, impostor, expect, t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong public key under the same ID: %v", err)
	}
}

func TestNonCanonicalEncodingsAreRejected(t *testing.T) {
	key := newKey(t)
	nonce, challenge := nonces()
	token, _, _ := newSigner(t, key).Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	expect := Expectation{NodeID: nodeA, Nonce: nonce, Challenge: challenge}
	payloadPart, signature, _ := strings.Cut(token, ".")
	for name, candidate := range map[string]string{
		"padding":   payloadPart + "=." + signature,
		"std chars": strings.NewReplacer("-", "+", "_", "/").Replace(token),
		"extra dot": token + ".",
		"empty":     "",
		"truncated": payloadPart[:len(payloadPart)-4] + "." + signature,
	} {
		if candidate == token {
			continue
		}
		if _, err := Verify(candidate, keysOf(key), expect, t0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSignerRejectsInvalidInputAndOfflineWindow(t *testing.T) {
	key := newKey(t)
	if _, err := NewSigner(key, MaxOfflineWindow+time.Second); err == nil {
		t.Fatal("offline window above 30 days accepted")
	}
	if _, err := NewSigner(key, 0); err == nil {
		t.Fatal("zero offline window accepted")
	}
	short, err := NewSigner(key, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	short.now = func() time.Time { return t0 }
	nonce, challenge := nonces()
	_, claims, _ := short.Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	if !claims.OfflineUntil.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("shorter Core policy not applied: %v", claims.OfflineUntil)
	}
	for name, args := range map[string][3]string{
		"bad node":   {"node_x", member1, string(memberships.RoleMember)},
		"bad member": {nodeA, "member_x", string(memberships.RoleMember)},
		"bad role":   {nodeA, member1, "OWNER"},
	} {
		if _, _, err := short.Issue(args[0], args[1], memberships.Role(args[2]), nonce, challenge); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseHex32AndCallbackURLs(t *testing.T) {
	if _, ok := ParseHex32(strings.Repeat("ab", 32)); !ok {
		t.Fatal("valid lowercase hex rejected")
	}
	for _, bad := range []string{strings.Repeat("AB", 32), strings.Repeat("a", 63), strings.Repeat("g", 64), ""} {
		if _, ok := ParseHex32(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"https://node.example/login/callback", "https://192.168.1.5:8443/login/callback",
		"https://xn--rknet-4ua.example/cb", "https://[2001:db8::1]:8443/cb", "https://node-a.lan/cb",
		"http://127.0.0.1:5173/login/callback", "http://localhost:5173/cb", "http://[::1]:8086/cb"} {
		if !ValidCallbackURL(good) {
			t.Errorf("%s rejected", good)
		}
	}
	for _, bad := range []string{
		// Hosts must be plain ASCII DNS names or IP literals: the origin is
		// shown to the user and written into the page's CSP.
		"https://*/cb", "https://*.evil.example/cb", "https://a;sandbox/cb", "https://a'self'/cb",
		"https://a,b/cb", "https://a=b/cb", "https://%C3%A9.example/cb", "https://%D0%B0rknet.example/cb",
		"https://a..example/cb", "https://-a.example/cb", "https://a.example./cb", "https://a_b.example/cb",
		"https://[fe80::1%25en0]/cb",
		// Ports must be 1-65535 when present.
		"https://a.example:/cb", "https://a.example:0/cb", "https://a.example:99999/cb", "https://a.example:08443/cb",
		"http://node.example/cb", "https://node.example", "https://node.example/cb?x=1",
		"https://node.example/cb#frag", "https://user@node.example/cb", "javascript:alert(1)", "/relative",
		"https://node.example/../cb", "HTTPS://node.example/cb", "https://node.example/" + strings.Repeat("a", 600)} {
		if ValidCallbackURL(bad) {
			t.Errorf("%s accepted", bad)
		}
	}
}

// Even with a valid signature, a payload whose fields are inconsistent with
// this format version is rejected.
func TestSignedButInconsistentPayloadsAreRejected(t *testing.T) {
	key := newKey(t)
	nonce, challenge := nonces()
	token, _, _ := newSigner(t, key).Issue(nodeA, member1, memberships.RoleMember, nonce, challenge)
	payloadPart, _, _ := strings.Cut(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(payloadPart)
	put := func(p []byte, offset int, at time.Time) {
		for i := 0; i < 8; i++ {
			p[offset+i] = byte(uint64(at.Unix()) >> (56 - 8*i))
		}
	}
	for name, mutate := range map[string]func([]byte){
		"longer lifetime":      func(p []byte) { put(p, offsetExpiresAt, t0.Add(Lifetime+time.Second)) },
		"offline beyond D3":    func(p []byte) { put(p, offsetOffline, t0.Add(MaxOfflineWindow+time.Second)) },
		"offline before issue": func(p []byte) { put(p, offsetOffline, t0) },
		"unknown purpose":      func(p []byte) { p[offsetPurpose] = 2 },
		"unknown version":      func(p []byte) { p[offsetVersion] = 2 },
		"unknown role":         func(p []byte) { p[offsetRole] = 3 },
		"extra permission":     func(p []byte) { p[offsetPermissions] = 0x03 },
		"no permission":        func(p []byte) { p[offsetPermissions] = 0 },
		"time near MaxInt64": func(p []byte) {
			for _, o := range []int{offsetIssuedAt, offsetExpiresAt, offsetOffline} {
				p[o] = 0x7f
			}
		},
		"time with top bit": func(p []byte) { p[offsetIssuedAt] |= 0x80 },
	} {
		changed := append([]byte(nil), payload...)
		mutate(changed)
		signature := ed25519.Sign(key, append([]byte(Domain), changed...))
		forged := base64.RawURLEncoding.EncodeToString(changed) + "." + base64.RawURLEncoding.EncodeToString(signature)
		if _, err := Verify(forged, keysOf(key), Expectation{NodeID: nodeA, Nonce: nonce, Challenge: challenge}, t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
}
