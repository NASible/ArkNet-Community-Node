package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"
)

// Relay revocation tokens (Prompt 48B). The layout is defined by the relay
// (relay/internal/revocation and relay/README.md); a golden test in both
// modules keeps the two byte-compatible.
const (
	relayRevocationDomain      = "ARKNET-RELAY-REVOCATION-V1\x00"
	maxRelayRevocationSubjects = 4096
)

func signRelayRevocation(key ed25519.PrivateKey, relayID string, id [16]byte, issuedAt time.Time, subjects [][16]byte) (string, error) {
	if len(subjects) == 0 || len(subjects) > maxRelayRevocationSubjects || relayID == "" || len(key) != ed25519.PrivateKeySize {
		return "", errors.New("invalid relay revocation")
	}
	payload := make([]byte, 59, 59+16*len(subjects))
	payload[0] = 1
	binary.BigEndian.PutUint64(payload[1:9], uint64(issuedAt.Unix()))
	copy(payload[9:25], id[:])
	audience := sha256.Sum256([]byte(relayID))
	copy(payload[25:57], audience[:])
	binary.BigEndian.PutUint16(payload[57:59], uint16(len(subjects)))
	for _, subject := range subjects {
		payload = append(payload, subject[:]...)
	}
	signature := ed25519.Sign(key, append([]byte(relayRevocationDomain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
