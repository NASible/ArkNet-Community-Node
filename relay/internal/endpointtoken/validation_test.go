package endpointtoken

import (
	"crypto/ed25519"
	"testing"
	"time"
)

func TestSignRejectsInvalidInput(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	badKey := append(ed25519.PrivateKey(nil), key...)
	badKey[63] ^= 1
	for _, tc := range []struct {
		name     string
		key      ed25519.PrivateKey
		endpoint string
		expires  time.Time
	}{
		{"missing key", nil, "100.64.1.2:6881", time.Unix(1800000000, 0)},
		{"short key", key[:32], "100.64.1.2:6881", time.Unix(1800000000, 0)},
		{"inconsistent key", badKey, "100.64.1.2:6881", time.Unix(1800000000, 0)},
		{"zero expiry", key, "100.64.1.2:6881", time.Unix(0, 0)},
		{"negative expiry", key, "100.64.1.2:6881", time.Unix(-1, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := Sign(tc.key, tc.endpoint, [16]byte{}, [32]byte{}, tc.expires)
			if err == nil || token != "" {
				t.Fatal("invalid input signed")
			}
		})
	}
	for _, endpoint := range []string{"", "example.com:6881", "100.63.255.255:6881", "100.128.0.0:6881", "127.0.0.1:6881", "[::ffff:100.64.1.2]:6881", "[::1]:6881", "100.64.1.2:0", "100.64.1.2:1023", "100.64.1.2:65536", "100.64.1.2:06881", "100.064.1.2:6881", "100.64.1.2:+6881", "100.64.1.2:6881 "} {
		t.Run(endpoint, func(t *testing.T) {
			token, err := Sign(key, endpoint, [16]byte{}, [32]byte{}, time.Unix(1800000000, 0))
			if err == nil || token != "" {
				t.Fatal("invalid endpoint signed")
			}
		})
	}
}

func TestSignEndpointBoundaries(t *testing.T) {
	for _, endpoint := range []string{"100.64.0.0:1024", "100.127.255.255:65535"} {
		if _, err := Sign(ed25519.NewKeyFromSeed(make([]byte, 32)), endpoint, [16]byte{}, [32]byte{}, time.Unix(1800000000, 0)); err != nil {
			t.Fatal(err)
		}
	}
}
