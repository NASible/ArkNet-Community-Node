package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type trackerGrantIssuer struct {
	announceBase *url.URL
	privateKey   ed25519.PrivateKey
	now          func() time.Time
}

func loadTrackerGrantIssuer(announceURL, keyFile string) (*trackerGrantIssuer, error) {
	if announceURL == "" && keyFile == "" {
		return nil, nil
	}
	if announceURL == "" || keyFile == "" {
		return nil, fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL and ARKNET_TRACKER_SIGNING_KEY_FILE must be configured together")
	}
	base, err := parseTrackerAnnounceBase(announceURL)
	if err != nil {
		return nil, err
	}
	key, err := os.Open(keyFile)
	if err != nil {
		return nil, fmt.Errorf("open ARKNET_TRACKER_SIGNING_KEY_FILE: %w", err)
	}
	defer key.Close()
	const encodedPrivateKeySize = 88
	encodedKey, err := io.ReadAll(io.LimitReader(key, encodedPrivateKeySize+1))
	if err != nil {
		return nil, fmt.Errorf("read ARKNET_TRACKER_SIGNING_KEY_FILE: %w", err)
	}
	if len(encodedKey) != encodedPrivateKeySize {
		return nil, fmt.Errorf("ARKNET_TRACKER_SIGNING_KEY_FILE must contain a base64-encoded 64-byte Ed25519 private key")
	}
	privateKey, err := base64.StdEncoding.Strict().DecodeString(string(encodedKey))
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("ARKNET_TRACKER_SIGNING_KEY_FILE must contain a base64-encoded 64-byte Ed25519 private key")
	}
	derivedPrivateKey := ed25519.NewKeyFromSeed(privateKey[:ed25519.SeedSize])
	if subtle.ConstantTimeCompare(privateKey, derivedPrivateKey) != 1 {
		return nil, fmt.Errorf("ARKNET_TRACKER_SIGNING_KEY_FILE contains an inconsistent Ed25519 private key")
	}
	return &trackerGrantIssuer{announceBase: base, privateKey: ed25519.PrivateKey(privateKey), now: time.Now}, nil
}

const (
	trackerGrantLifetime       = 5 * time.Minute
	trackerGrantActionDownload = byte(0x01)
	trackerGrantActionSeed     = byte(0x02)
	trackerGrantActionMask     = trackerGrantActionDownload | trackerGrantActionSeed
)

func (i *trackerGrantIssuer) issue(nodeID, infoHashV2 string, actionMask byte) (string, time.Time, error) {
	grant, expiresAt, err := i.issueGrant(nodeID, infoHashV2, actionMask)
	if err != nil {
		return "", time.Time{}, err
	}
	trackerURL := *i.announceBase
	trackerURL.Path = "/announce/" + grant
	return trackerURL.String(), expiresAt, nil
}

func (i *trackerGrantIssuer) issueGrant(nodeID, infoHashV2 string, actionMask byte) (string, time.Time, error) {
	if !validInfoHashV2(infoHashV2) {
		return "", time.Time{}, fmt.Errorf("invalid info hash v2")
	}
	if actionMask == 0 || actionMask&^trackerGrantActionMask != 0 {
		return "", time.Time{}, fmt.Errorf("invalid tracker grant action mask")
	}
	hash, err := hex.DecodeString(infoHashV2)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("decode info hash v2: %w", err)
	}
	expiresAt := i.now().UTC().Add(trackerGrantLifetime)
	payload := make([]byte, 1+8+16+len(hash)+1)
	payload[0] = 2
	binary.BigEndian.PutUint64(payload[1:9], uint64(expiresAt.Unix()))
	subjectMessage := append([]byte("arknet-tracker-subject-v1\x00"+nodeID+"\x00"), hash...)
	subject := sha256.Sum256(ed25519.Sign(i.privateKey, subjectMessage))
	copy(payload[9:25], subject[:16])
	copy(payload[25:57], hash)
	payload[57] = actionMask
	signature := ed25519.Sign(i.privateKey, payload)
	grant := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
	return grant, expiresAt, nil
}

func (i *trackerGrantIssuer) verifyGrant(grant, infoHashV2 string, requiredAction byte) bool {
	hash, ok := i.verifyGrantClaims(grant, requiredAction)
	if !ok || !validInfoHashV2(infoHashV2) {
		return false
	}
	expected, err := hex.DecodeString(infoHashV2)
	return err == nil && subtle.ConstantTimeCompare(hash, expected) == 1
}

func (i *trackerGrantIssuer) verifyGrantClaims(grant string, requiredAction byte) ([]byte, bool) {
	if i == nil || requiredAction == 0 || requiredAction&^trackerGrantActionMask != 0 {
		return nil, false
	}
	parts := strings.Split(grant, ".")
	if len(parts) != 2 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payload) != 58 || base64.RawURLEncoding.EncodeToString(payload) != parts[0] || payload[0] != 2 {
		return nil, false
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return nil, false
	}
	publicKey, ok := i.privateKey.Public().(ed25519.PublicKey)
	if !ok || !ed25519.Verify(publicKey, payload, signature) {
		return nil, false
	}
	expiresUnix := binary.BigEndian.Uint64(payload[1:9])
	if expiresUnix > uint64(^uint64(0)>>1) || !i.now().UTC().Before(time.Unix(int64(expiresUnix), 0)) {
		return nil, false
	}
	if payload[57]&requiredAction != requiredAction || payload[57] == 0 || payload[57]&^trackerGrantActionMask != 0 {
		return nil, false
	}
	return payload[25:57], true
}

func validInfoHashV2(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func parseTrackerAnnounceBase(value string) (*url.URL, error) {
	base, err := url.Parse(value)
	if err != nil || base.Host == "" || base.Hostname() == "" || base.User != nil || base.Opaque != "" || base.RawPath != "" || (base.Path != "" && base.Path != "/") || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL must be an origin URL without userinfo, path, query, or fragment")
	}
	if err := validateTrackerAuthority(base); err != nil {
		return nil, err
	}
	if base.Scheme != "https" {
		host := base.Hostname()
		ip := net.ParseIP(host)
		allowedHTTP := base.Scheme == "http" && (strings.EqualFold(host, "localhost") || strings.EqualFold(host, "arknet-tracker") || ip != nil && ip.IsLoopback())
		if !allowedHTTP {
			return nil, fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL must use HTTPS except for loopback or arknet-tracker")
		}
	}
	base.Path = ""
	return base, nil
}

func validateTrackerAuthority(base *url.URL) error {
	host := base.Hostname()
	if strings.Contains(host, "%") {
		return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL must not contain an IPv6 zone or encoded host")
	}
	if net.ParseIP(host) == nil {
		if strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") || strings.Contains(host, "..") {
			return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL contains an invalid hostname")
		}
		for _, c := range host {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
				return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL contains an invalid hostname")
			}
		}
	}

	authority := base.Host
	var port string
	if strings.HasPrefix(authority, "[") {
		closingBracket := strings.LastIndex(authority, "]")
		if closingBracket < 0 {
			return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL contains an invalid IPv6 authority")
		}
		remainder := authority[closingBracket+1:]
		if remainder != "" {
			if !strings.HasPrefix(remainder, ":") {
				return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL contains an invalid authority")
			}
			port = remainder[1:]
		}
	} else if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
		port = authority[colon+1:]
	}
	if strings.HasSuffix(authority, ":") {
		return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL contains an empty port")
	}
	if port != "" {
		for _, c := range port {
			if c < '0' || c > '9' {
				return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL port must be decimal")
			}
		}
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("ARKNET_TRACKER_ANNOUNCE_URL port must be between 1 and 65535")
		}
	}
	return nil
}
