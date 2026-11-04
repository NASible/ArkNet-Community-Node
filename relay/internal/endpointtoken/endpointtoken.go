// Package endpointtoken signs relay endpoint attestations for tracker announces.
package endpointtoken

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

const domain = "ARKNET-RELAY-ENDPOINT-V1\x00"

// Sign binds an IPv4 CGNAT endpoint to a seed grant's subject, full hash and expiry.
// The caller must authenticate the seed grant before issuance.
func Sign(key ed25519.PrivateKey, endpoint string, subject [16]byte, hash [32]byte, expires time.Time) (string, error) {
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(key, ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])) {
		return "", fmt.Errorf("invalid endpoint signing key")
	}
	address, err := netip.ParseAddrPort(endpoint)
	if err != nil || !address.Addr().Is4() || !netip.MustParsePrefix("100.64.0.0/10").Contains(address.Addr()) || address.Port() < 1024 || address.String() != endpoint {
		return "", fmt.Errorf("invalid relay endpoint")
	}
	if expires.Unix() <= 0 {
		return "", fmt.Errorf("invalid endpoint expiry")
	}
	payload := make([]byte, 63)
	payload[0] = 1
	binary.BigEndian.PutUint64(payload[1:9], uint64(expires.Unix()))
	copy(payload[9:25], subject[:])
	copy(payload[25:57], hash[:])
	ip := address.Addr().As4()
	copy(payload[57:61], ip[:])
	binary.BigEndian.PutUint16(payload[61:63], address.Port())
	signature := ed25519.Sign(key, append([]byte(domain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
