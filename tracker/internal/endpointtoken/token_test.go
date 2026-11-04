package endpointtoken

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

func signed(key ed25519.PrivateKey, payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte("ARKNET-RELAY-ENDPOINT-V1\x00"), payload...)))
}
func TestVerifyEndpointClaims(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	var subject [16]byte
	subject[0] = 1
	var hash [32]byte
	hash[0] = 2
	p := make([]byte, 63)
	p[0] = 1
	binary.BigEndian.PutUint64(p[1:9], uint64(now.Add(time.Minute).Unix()))
	copy(p[9:25], subject[:])
	copy(p[25:57], hash[:])
	copy(p[57:61], []byte{100, 64, 1, 2})
	binary.BigEndian.PutUint16(p[61:63], 6881)
	claims, err := Verify(signed(key, p), []ed25519.PublicKey{pub}, subject, hash, now)
	if err != nil || claims.Endpoint != [6]byte{100, 64, 1, 2, 0x1a, 0xe1} || claims.ExpiresUnix != now.Add(time.Minute).Unix() {
		t.Fatalf("%+v %v", claims, err)
	}
}
func TestRejectsInvalidTokensIncludingResignedClaims(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	var subject [16]byte
	subject[0] = 1
	var hash [32]byte
	hash[31] = 2
	p := make([]byte, 63)
	p[0] = 1
	binary.BigEndian.PutUint64(p[1:9], uint64(now.Add(time.Minute).Unix()))
	copy(p[9:25], subject[:])
	copy(p[25:57], hash[:])
	copy(p[57:61], []byte{100, 127, 255, 254})
	binary.BigEndian.PutUint16(p[61:63], 6881)
	mutations := map[string]func([]byte) []byte{
		"version":                     func(b []byte) []byte { b[0] = 2; return b },
		"expired boundary":            func(b []byte) []byte { binary.BigEndian.PutUint64(b[1:9], uint64(now.Unix())); return b },
		"subject":                     func(b []byte) []byte { b[9] ^= 1; return b },
		"full hash":                   func(b []byte) []byte { b[56] ^= 1; return b },
		"outside prefix":              func(b []byte) []byte { b[57] = 192; return b },
		"outside prefix second octet": func(b []byte) []byte { b[58] = 128; return b },
		"port zero":                   func(b []byte) []byte { binary.BigEndian.PutUint16(b[61:63], 0); return b },
		"privileged port":             func(b []byte) []byte { binary.BigEndian.PutUint16(b[61:63], 1023); return b },
		"short":                       func(b []byte) []byte { return b[:62] },
		"trailing":                    func(b []byte) []byte { return append(b, 0) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(signed(key, mutate(append([]byte(nil), p...))), []ed25519.PublicKey{pub}, subject, hash, now); err == nil {
				t.Fatal("accepted invalid claim")
			}
		})
	}
	for name, token := range map[string]string{"wrong signer": signed(other, p), "garbage": "x.y", "oversized": string(make([]byte, 513)), "extra part": signed(key, p) + ".x", "embedded newline": signed(key, p)[:12] + "\n" + signed(key, p)[12:], "unsigned": base64.RawURLEncoding.EncodeToString(p) + "."} {
		t.Run(name, func(t *testing.T) {
			if _, err := Verify(token, []ed25519.PublicKey{pub}, subject, hash, now); err == nil {
				t.Fatal("accepted invalid token")
			}
		})
	}
	if _, err := Verify(signed(key, p), nil, subject, hash, now); err == nil {
		t.Fatal("accepted with no trusted keys")
	}
	if _, err := Verify(signed(key, p), []ed25519.PublicKey{{1}}, subject, hash, now); err == nil {
		t.Fatal("accepted malformed public key")
	}
	// Any configured relay may sign; do not accidentally trust just the first.
	otherPub := other.Public().(ed25519.PublicKey)
	if _, err := Verify(signed(other, p), []ed25519.PublicKey{pub, otherPub}, subject, hash, now); err != nil {
		t.Fatal(err)
	}
}
