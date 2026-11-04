package tracker

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// Independent test signer. The relay's actual signer is exercised by the
// cross-module golden and live registration tests, not substituted in them.
func endpointToken(key ed25519.PrivateKey, hash [32]byte, subject [16]byte, expires time.Time, endpoint [6]byte) string {
	p := make([]byte, 63)
	p[0] = 1
	binary.BigEndian.PutUint64(p[1:9], uint64(expires.Unix()))
	copy(p[9:25], subject[:])
	copy(p[25:57], hash[:])
	copy(p[57:63], endpoint[:])
	return base64.RawURLEncoding.EncodeToString(p) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte("ARKNET-RELAY-ENDPOINT-V1\x00"), p...)))
}
func withEndpoint(query, token string) string {
	v, _ := url.ParseQuery(query)
	v.Set("endpoint_token", token)
	return v.Encode()
}

func TestOriginsNeverAdvertisedIPv4OrIPv6(t *testing.T) {
	for _, origin := range []string{"192.0.2.10:54321", "[2001:db8::10]:54321", "opaque-proxy-origin"} {
		t.Run(origin, func(t *testing.T) {
			pub, key, _ := ed25519.GenerateKey(nil)
			relayPub, relayKey, _ := ed25519.GenerateKey(nil)
			now := time.Unix(1800000000, 0)
			hash := [32]byte{1}
			subject := subjectFor(1)
			h := NewHandler(pub, time.Minute, time.Minute, func() time.Time { return now }, relayPub)
			endpoint := [6]byte{100, 64, 1, 2, 0x1a, 0xe1}
			token := endpointToken(relayKey, hash, subject, now.Add(time.Minute), endpoint)
			seed := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subject), withEndpoint(seedQuery(hash, "-ARKNET-PEER-0000001", "9999"), token), origin)
			if seed.Code != 200 {
				t.Fatalf("seed: %d %s", seed.Code, seed.Body)
			}
			response := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subjectFor(2)), announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "[2001:db8::20]:50000")
			want := append([]byte("d8:completei1e10:incompletei1e8:intervali60e5:peers6:"), endpoint[:]...)
			want = append(want, []byte("6:peers60:e")...)
			if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), want) {
				t.Fatalf("response %d %q want %q", response.Code, response.Body.Bytes(), want)
			}
			host, _, _ := net.SplitHostPort(origin)
			ip := net.ParseIP(host)
			if ip != nil && (bytes.Contains(response.Body.Bytes(), ip.To16()) || (ip.To4() != nil && bytes.Contains(response.Body.Bytes(), ip.To4()))) {
				t.Fatal("origin disclosed")
			}
		})
	}
}
func TestNoEndpointTokenAllowsOnlyDownloaders(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	hash := [32]byte{1}
	h := NewHandler(pub, time.Minute, time.Minute, func() time.Time { return now })
	token := makeGrant(t, key, hash, now.Add(time.Minute))
	if r := announce(t, h, token, seedQuery(hash, "-ARKNET-PEER-0000001", "6881"), "192.0.2.1:1"); r.Code != http.StatusForbidden {
		t.Fatalf("tokenless seed %d %s", r.Code, r.Body)
	}
	if r := announce(t, h, token, announceQuery(hash, "-ARKNET-PEER-0000001", "6881"), "not-an-address"); r.Code != 200 {
		t.Fatalf("downloader %d %s", r.Code, r.Body)
	}
}
func TestInvalidEndpointRefusalDoesNotMutateSwarm(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	relayPub, relayKey, _ := ed25519.GenerateKey(nil)
	_, other, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	hash := [32]byte{1}
	differentHash := hash
	differentHash[31] = 2
	endpoint := [6]byte{100, 64, 1, 2, 0x1a, 0xe1}
	subject := subjectFor(1)
	for name, token := range map[string]string{
		"forged":                 endpointToken(other, hash, subject, now.Add(time.Minute), endpoint),
		"expired":                endpointToken(relayKey, hash, subject, now, endpoint),
		"other hash same prefix": endpointToken(relayKey, differentHash, subject, now.Add(time.Minute), endpoint),
		"other subject":          endpointToken(relayKey, hash, subjectFor(2), now.Add(time.Minute), endpoint),
		"empty":                  "",
	} {
		t.Run(name, func(t *testing.T) {
			h := NewHandler(pub, time.Minute, time.Minute, func() time.Time { return now }, relayPub)
			grant := makeGrantForSubject(t, key, hash, now.Add(time.Minute), subject)
			valid := endpointToken(relayKey, hash, subject, now.Add(time.Minute), endpoint)
			if r := announce(t, h, grant, withEndpoint(seedQuery(hash, "-ARKNET-PEER-0000001", "6881"), valid), "192.0.2.1:1"); r.Code != 200 {
				t.Fatal(r.Code)
			}
			for _, query := range []string{seedQuery(hash, "-ARKNET-PEER-0000001", "6881"), announceQuery(hash, "-ARKNET-PEER-0000001", "6881")} {
				r := announce(t, h, grant, withEndpoint(query+"&event=stopped", token), "192.0.2.1:1")
				if r.Code != 403 {
					t.Fatalf("invalid token not rejected: %d %s", r.Code, r.Body)
				}
			}
			r := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subjectFor(3)), announceQuery(hash, "-ARKNET-PEER-0000003", "6883"), "192.0.2.3:3")
			if !bytes.Contains(r.Body.Bytes(), endpoint[:]) {
				t.Fatalf("denied announce removed seed %q", r.Body.Bytes())
			}
		})
	}
}
func TestEndpointExpiryPrunesBeforeGrantExpiry(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	relayPub, relayKey, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	hash := [32]byte{1}
	subject := subjectFor(1)
	endpoint := [6]byte{100, 64, 1, 2, 0x1a, 0xe1}
	h := NewHandler(pub, time.Minute, time.Hour, func() time.Time { return now }, relayPub)
	token := endpointToken(relayKey, hash, subject, now.Add(time.Second), endpoint)
	announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subject), withEndpoint(seedQuery(hash, "-ARKNET-PEER-0000001", "6881"), token), "192.0.2.1:1")
	now = now.Add(time.Second)
	r := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subjectFor(2)), announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.2:2")
	if bytes.Contains(r.Body.Bytes(), endpoint[:]) || !bytes.Contains(r.Body.Bytes(), []byte("8:completei0e")) {
		t.Fatalf("expired endpoint advertised %q", r.Body.Bytes())
	}
}
func TestTrackerRecordsHaveNoOriginAddressFields(t *testing.T) {
	// State schema is intentionally a compact virtual endpoint, not net.IP or
	// a remote address. This catches origin retention even without a response leak.
	for _, typ := range []reflect.Type{reflect.TypeOf(peer{}), reflect.TypeOf(announceRequest{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Name == "ip" || f.Name == "port" || f.Type == reflect.TypeOf(net.IP{}) {
				t.Errorf("origin field retained: %s.%s", typ, f.Name)
			}
		}
	}
}
