// Package assertions issues Core-signed, one-time Node login assertions
// (docs/global-login-assertions.md, ARKSEC-001).
//
// An assertion is base64url(payload) "." base64url(signature), both without
// padding. The signature is Ed25519 over Domain || payload, made with a key
// dedicated to this purpose (never the tracker, web-seed, enrollment or TLS
// key). The fixed 180-byte payload is:
//
//	offset size field
//	     0    1 version (1)
//	     1    1 purpose (1 = NODE_LOGIN)
//	     2    8 key ID (first 8 bytes of SHA-256 of the public key)
//	    10   32 node ID (raw bytes of node_<64 hex>)
//	    42   32 member ID (raw bytes of member_<64 hex>, pairwise for the Node)
//	    74   32 nonce (chosen by the Node)
//	   106   32 challenge (SHA-256 of the Node's login verifier)
//	   138   16 assertion ID (random)
//	   154    8 issued at (Unix seconds, big-endian)
//	   162    8 expires at (issued at + 120 s)
//	   170    1 role (1 = MEMBER, 2 = ADMIN)
//	   171    1 permissions (0x01 = MEDIA_ACCESS)
//	   172    8 offline until (at most issued at + 30 days)
package assertions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
)

const (
	Version               = 1
	PurposeNodeLogin      = 1
	PermissionMediaAccess = byte(0x01)
	Lifetime              = 120 * time.Second
	// ClockSkew applies only to the Core-clock timestamps in an assertion.
	ClockSkew = 60 * time.Second
	// MaxOfflineWindow is decision D3: offline login on a Node for at most 30
	// days since the last online verification.
	MaxOfflineWindow = 30 * 24 * time.Hour
	// MinOfflineWindow allows short windows for tests; shorter is only safer.
	MinOfflineWindow = time.Minute
	// NonceRetention keeps a claimed nonce longer than any Node pending login
	// (5 minutes) and any assertion's acceptance window.
	NonceRetention = 10 * time.Minute
	PayloadSize    = 180
	// MaxCallbackURLs bounds one Node's registered login callbacks.
	MaxCallbackURLs      = 4
	maxCallbackURLLength = 512
)

// Assertion timestamps must fall in 2020-01-01..2200-01-01 (Unix seconds).
const (
	minAssertionTime = 1577836800
	maxAssertionTime = 7258118400
)

// Domain separates these signatures from every other ArkNet signature.
const Domain = "ARKNET-NODE-LOGIN-V1\x00"

const (
	offsetVersion     = 0
	offsetPurpose     = 1
	offsetKeyID       = 2
	offsetNode        = 10
	offsetMember      = 42
	offsetNonce       = 74
	offsetChallenge   = 106
	offsetAssertionID = 138
	offsetIssuedAt    = 154
	offsetExpiresAt   = 162
	offsetRole        = 170
	offsetPermissions = 171
	offsetOffline     = 172
)

var (
	// ErrInvalid covers every malformed, forged, expired or mismatched assertion.
	ErrInvalid = errors.New("invalid login assertion")
	// ErrUnknownKey means the key ID is not in the published key set.
	ErrUnknownKey = errors.New("login assertion signed by an unpublished key")
	// ErrDenied covers unknown, unapproved or untrusted Nodes, missing
	// memberships, inactive users and unregistered callbacks.
	ErrDenied = errors.New("node login denied")
	// ErrNonceUsed means Core already issued an assertion for this Node nonce.
	ErrNonceUsed = errors.New("node login nonce already used")
)

// KeyID names a signing key in the published key set.
type KeyID [8]byte

func KeyIDFor(public ed25519.PublicKey) KeyID {
	sum := sha256.Sum256(public)
	var id KeyID
	copy(id[:], sum[:8])
	return id
}

func (k KeyID) String() string { return hex.EncodeToString(k[:]) }

// Claims is the decoded payload.
type Claims struct {
	KeyID        KeyID
	Purpose      byte
	NodeID       string
	MemberID     string
	Nonce        [32]byte
	Challenge    [32]byte
	AssertionID  [16]byte
	IssuedAt     time.Time
	ExpiresAt    time.Time
	OfflineUntil time.Time
	Role         memberships.Role
	Permissions  byte
}

// Signer issues assertions with the active key.
type Signer struct {
	key     ed25519.PrivateKey
	id      KeyID
	offline time.Duration
	now     func() time.Time
}

// NewSigner accepts an Ed25519 private key and Core's offline window, which may
// be shorter than, but never longer than, MaxOfflineWindow.
func NewSigner(key ed25519.PrivateKey, offline time.Duration) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("assertion signing key must be an Ed25519 private key")
	}
	if offline < MinOfflineWindow || offline > MaxOfflineWindow {
		return nil, fmt.Errorf("offline window must be between %s and %s", MinOfflineWindow, MaxOfflineWindow)
	}
	return &Signer{key: key, id: KeyIDFor(key.Public().(ed25519.PublicKey)), offline: offline, now: time.Now}, nil
}

func (s *Signer) KeyID() KeyID                 { return s.id }
func (s *Signer) OfflineWindow() time.Duration { return s.offline }
func (s *Signer) PublicKey() ed25519.PublicKey { return s.key.Public().(ed25519.PublicKey) }

// PrivateKey is exposed only so Core can refuse to reuse it for another purpose.
func (s *Signer) PrivateKey() ed25519.PrivateKey { return s.key }

// Issue signs one assertion. The caller must already have authenticated the
// user, checked the membership and claimed the nonce.
func (s *Signer) Issue(nodeID, memberID string, role memberships.Role, nonce, challenge [32]byte) (string, Claims, error) {
	node, nodeOK := nodeIDBytes(nodeID)
	member, memberOK := memberIDBytes(memberID)
	if !nodeOK || !memberOK || roleByte(role) == 0 {
		return "", Claims{}, ErrInvalid
	}
	issued := s.now().UTC().Truncate(time.Second)
	claims := Claims{KeyID: s.id, Purpose: PurposeNodeLogin, NodeID: nodeID, MemberID: memberID, Nonce: nonce,
		Challenge: challenge, IssuedAt: issued, ExpiresAt: issued.Add(Lifetime), OfflineUntil: issued.Add(s.offline),
		Role: role, Permissions: PermissionMediaAccess}
	if _, err := rand.Read(claims.AssertionID[:]); err != nil {
		return "", Claims{}, err
	}
	payload := make([]byte, PayloadSize)
	payload[offsetVersion] = Version
	payload[offsetPurpose] = PurposeNodeLogin
	copy(payload[offsetKeyID:], s.id[:])
	copy(payload[offsetNode:], node[:])
	copy(payload[offsetMember:], member[:])
	copy(payload[offsetNonce:], nonce[:])
	copy(payload[offsetChallenge:], challenge[:])
	copy(payload[offsetAssertionID:], claims.AssertionID[:])
	binary.BigEndian.PutUint64(payload[offsetIssuedAt:], uint64(claims.IssuedAt.Unix()))
	binary.BigEndian.PutUint64(payload[offsetExpiresAt:], uint64(claims.ExpiresAt.Unix()))
	payload[offsetRole] = roleByte(role)
	payload[offsetPermissions] = PermissionMediaAccess
	binary.BigEndian.PutUint64(payload[offsetOffline:], uint64(claims.OfflineUntil.Unix()))
	signature := ed25519.Sign(s.key, append([]byte(Domain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), claims, nil
}

// Expectation is what the verifying Node knows independently of the assertion.
type Expectation struct {
	NodeID    string
	Nonce     [32]byte
	Challenge [32]byte
}

// Verify checks encoding, key, signature, purpose, binding and time. It is the
// reference for the Node-side verifier (Prompt 46B); Core uses it in tests.
func Verify(token string, keys map[KeyID]ed25519.PublicKey, expect Expectation, now time.Time) (Claims, error) {
	payloadPart, signaturePart, found := strings.Cut(token, ".")
	if !found || strings.Contains(signaturePart, ".") {
		return Claims{}, ErrInvalid
	}
	payload, ok := decodeCanonical(payloadPart, PayloadSize)
	signature, sigOK := decodeCanonical(signaturePart, ed25519.SignatureSize)
	if !ok || !sigOK || payload[offsetVersion] != Version {
		return Claims{}, ErrInvalid
	}
	var claims Claims
	copy(claims.KeyID[:], payload[offsetKeyID:])
	public, known := keys[claims.KeyID]
	if !known {
		return Claims{}, ErrUnknownKey
	}
	if len(public) != ed25519.PublicKeySize || !ed25519.Verify(public, append([]byte(Domain), payload...), signature) {
		return Claims{}, ErrInvalid
	}
	claims.Purpose = payload[offsetPurpose]
	claims.NodeID = "node_" + hex.EncodeToString(payload[offsetNode:offsetMember])
	claims.MemberID = "member_" + hex.EncodeToString(payload[offsetMember:offsetNonce])
	copy(claims.Nonce[:], payload[offsetNonce:])
	copy(claims.Challenge[:], payload[offsetChallenge:])
	copy(claims.AssertionID[:], payload[offsetAssertionID:])
	for _, offset := range []int{offsetIssuedAt, offsetExpiresAt, offsetOffline} {
		if seconds := binary.BigEndian.Uint64(payload[offset:]); seconds < minAssertionTime || seconds > maxAssertionTime {
			return Claims{}, ErrInvalid
		}
	}
	claims.IssuedAt = unixAt(payload[offsetIssuedAt:])
	claims.ExpiresAt = unixAt(payload[offsetExpiresAt:])
	claims.OfflineUntil = unixAt(payload[offsetOffline:])
	claims.Role = roleOf(payload[offsetRole])
	claims.Permissions = payload[offsetPermissions]
	switch {
	case claims.Purpose != PurposeNodeLogin, claims.Role == "", claims.Permissions != PermissionMediaAccess,
		claims.NodeID != expect.NodeID,
		subtle.ConstantTimeCompare(claims.Nonce[:], expect.Nonce[:]) != 1,
		subtle.ConstantTimeCompare(claims.Challenge[:], expect.Challenge[:]) != 1,
		claims.ExpiresAt.Sub(claims.IssuedAt) != Lifetime,
		!claims.OfflineUntil.After(claims.IssuedAt), claims.OfflineUntil.Sub(claims.IssuedAt) > MaxOfflineWindow,
		claims.IssuedAt.After(now.Add(ClockSkew)), !now.Before(claims.ExpiresAt.Add(ClockSkew)):
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

// ParseHex32 accepts exactly 64 lowercase hexadecimal characters.
func ParseHex32(value string) ([32]byte, bool) {
	var out [32]byte
	if len(value) != 64 || strings.ToLower(value) != value {
		return out, false
	}
	if _, err := hex.Decode(out[:], []byte(value)); err != nil {
		return out, false
	}
	return out, true
}

// ValidCallbackURL accepts a canonical absolute HTTPS URL with a path, or plain
// HTTP only for a loopback host (local development). It rejects user info,
// queries, fragments and dot segments.
func ValidCallbackURL(raw string) bool {
	if len(raw) == 0 || len(raw) > maxCallbackURLLength {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.String() != raw || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Opaque != "" || u.Host == "" || u.Hostname() == "" || strings.ToLower(u.Host) != u.Host {
		return false
	}
	if u.Path == "" || u.Path == "/" || path.Clean(u.Path) != u.Path || u.RawPath != "" || !validCallbackHost(u) {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		host := u.Hostname()
		ip := net.ParseIP(host)
		return host == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return false
}

// validCallbackHost accepts an ASCII DNS name (letters, digits and hyphens in
// non-empty labels; IDNs must be punycode) or an IP literal without a zone,
// and a port of 1-65535 without leading zeros when present. The origin is
// shown to the user and written into the login page's CSP, so nothing else may
// reach it.
func validCallbackHost(u *url.URL) bool {
	if port := u.Port(); port != "" || strings.HasSuffix(u.Host, ":") {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return false
		}
	}
	host := u.Hostname()
	if strings.HasPrefix(u.Host, "[") {
		ip, err := netip.ParseAddr(host)
		return err == nil && ip.Is6() && ip.Zone() == ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Is4()
	}
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// CallbackOrigin is scheme://host[:port] of a valid callback URL.
func CallbackOrigin(raw string) string {
	u, _ := url.Parse(raw)
	return u.Scheme + "://" + u.Host
}

// Membership is the authenticated user's pairwise identity on one Node.
type Membership struct {
	MemberID string
	Role     memberships.Role
}

// Repository is Core's storage for the login flow.
//
// LoginCallbacks and LoginMembership return ErrDenied unless the Node is
// APPROVED and TRUSTED (and, for LoginMembership, the user is ACTIVE and a
// member of that Node). ClaimLoginNonce atomically records a nonce digest for
// the Node, kept for retention by the store's clock, and returns ErrNonceUsed if it was already claimed and has not yet
// been pruned; expired claims may be pruned. SetLoginCallbacks replaces the
// Node's callback set and returns ErrDenied for an unapproved Node.
type Repository interface {
	LoginCallbacks(ctx context.Context, nodeID string) ([]string, error)
	SetLoginCallbacks(ctx context.Context, nodeID string, callbackURLs []string) error
	LoginMembership(ctx context.Context, nodeID, userID string) (Membership, error)
	ClaimLoginNonce(ctx context.Context, nodeID string, nonceDigest [32]byte, retention time.Duration) error
}

func decodeCanonical(part string, size int) ([]byte, bool) {
	data, err := base64.RawURLEncoding.Strict().DecodeString(part)
	if err != nil || len(data) != size || base64.RawURLEncoding.EncodeToString(data) != part {
		return nil, false
	}
	return data, true
}

func unixAt(b []byte) time.Time {
	return time.Unix(int64(binary.BigEndian.Uint64(b)), 0).UTC()
}

func nodeIDBytes(id string) ([32]byte, bool) {
	if !nodes.ValidPublicID(id) {
		return [32]byte{}, false
	}
	return ParseHex32(strings.TrimPrefix(id, "node_"))
}

func memberIDBytes(id string) ([32]byte, bool) {
	if !memberships.ValidMemberID(id) {
		return [32]byte{}, false
	}
	return ParseHex32(strings.TrimPrefix(id, "member_"))
}

func roleByte(role memberships.Role) byte {
	switch role {
	case memberships.RoleMember:
		return 1
	case memberships.RoleAdmin:
		return 2
	}
	return 0
}

func roleOf(b byte) memberships.Role {
	switch b {
	case 1:
		return memberships.RoleMember
	case 2:
		return memberships.RoleAdmin
	}
	return ""
}
