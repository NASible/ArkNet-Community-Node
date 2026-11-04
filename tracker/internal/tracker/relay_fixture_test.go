package tracker

import (
	"crypto/ed25519"
	"strconv"
	"time"
)

func testRelayPrivateKey() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(make([]byte, 32)) }
func testRelayPublicKey() ed25519.PublicKey {
	return testRelayPrivateKey().Public().(ed25519.PublicKey)
}
func signedSeedQuery(hash [32]byte, subject [16]byte, expires time.Time, peerID, portText string) string {
	port, _ := strconv.Atoi(portText)
	endpoint := [6]byte{100, 64, 1, 2, byte(port >> 8), byte(port)}
	return withEndpoint(seedQuery(hash, peerID, portText), endpointToken(testRelayPrivateKey(), hash, subject, expires, endpoint))
}
