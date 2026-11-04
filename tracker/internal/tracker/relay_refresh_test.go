package tracker

import (
	"bytes"
	"crypto/ed25519"
	"net/url"
	"testing"
	"time"
)

func TestEndpointAnnounceRenewalAndShorterGrantExpiry(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	hash := [32]byte{5}
	subject := subjectFor(1)
	h := NewHandler(pub, time.Minute, time.Hour, func() time.Time { return now }, testRelayPublicKey())
	seedGrant := makeGrantForSubject(t, key, hash, now.Add(5*time.Second), subject)
	query := signedSeedQuery(hash, subject, now.Add(2*time.Second), "-ARKNET-PEER-0000001", "6881")
	if r := announce(t, h, seedGrant, query, "[2001:db8::1]:1"); r.Code != 200 {
		t.Fatal(r.Code)
	}
	// A fresh endpoint token from control renewal extends the advertisement,
	// but never past the Core grant that authorized this announce.
	now = now.Add(time.Second)
	query = signedSeedQuery(hash, subject, now.Add(time.Minute), "-ARKNET-PEER-0000001", "6881")
	if r := announce(t, h, seedGrant, query, "192.0.2.1:1"); r.Code != 200 {
		t.Fatal(r.Code)
	}
	observe := func() []byte {
		t.Helper()
		r := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subjectFor(2)), announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.2:2")
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
		return r.Body.Bytes()
	}
	now = now.Add(time.Second)
	if !bytes.Contains(observe(), []byte{100, 64, 1, 2, 0x1a, 0xe1}) {
		t.Fatal("renewed endpoint lost at original token expiry")
	}
	now = now.Add(3 * time.Second)
	if bytes.Contains(observe(), []byte{100, 64, 1, 2, 0x1a, 0xe1}) {
		t.Fatal("endpoint outlived Core grant")
	}
}
func TestDuplicateOrOversizedEndpointParameterRejected(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	hash := [32]byte{1}
	h := NewHandler(pub, time.Minute, time.Hour, func() time.Time { return now }, testRelayPublicKey())
	token := makeGrant(t, key, hash, now.Add(time.Minute))
	for _, values := range [][]string{{"one", "two"}, {string(make([]byte, 257))}} {
		q, _ := url.ParseQuery(announceQuery(hash, "-ARKNET-PEER-0000001", "6881"))
		q["endpoint_token"] = values
		if r := announce(t, h, token, q.Encode(), "192.0.2.1:1"); r.Code != 400 {
			t.Fatalf("status %d", r.Code)
		}
	}
}
func TestValidEndpointOnDownloaderStillNeverAdvertised(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Unix(1800000000, 0)
	hash := [32]byte{1}
	subject := subjectFor(1)
	h := NewHandler(pub, time.Minute, time.Hour, func() time.Time { return now }, testRelayPublicKey())
	q, _ := url.ParseQuery(signedSeedQuery(hash, subject, now.Add(time.Minute), "-ARKNET-PEER-0000001", "6881"))
	q.Set("left", "1")
	if r := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subject), q.Encode(), "192.0.2.1:1"); r.Code != 200 {
		t.Fatal(r.Code)
	}
	r := announce(t, h, makeGrantForSubject(t, key, hash, now.Add(time.Minute), subjectFor(2)), announceQuery(hash, "-ARKNET-PEER-0000002", "6882"), "192.0.2.2:2")
	if !bytes.Contains(r.Body.Bytes(), []byte("5:peers0:6:peers60:")) {
		t.Fatalf("downloader advertised %q", r.Body.Bytes())
	}
}
