package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

func TestClaims(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	c := claims{Kind: "grant-v1", Expiry: time.Now().Add(time.Second).UnixNano()}
	s := sign(key, c)
	if _, e := verify(pub, s, "grant-v1"); e != nil {
		t.Fatal(e)
	}
	s.Sig[0] ^= 1
	if _, e := verify(pub, s, "grant-v1"); e == nil {
		t.Fatal("tamper accepted")
	}
	c.Expiry = time.Now().UnixNano()
	if _, e := verify(pub, sign(key, c), "grant-v1"); e == nil {
		t.Fatal("expiry accepted")
	}
	c.Expiry = time.Now().Add(time.Second).UnixNano()
	if _, e := verify(pub, sign(key, c), "admission-v1"); e == nil {
		t.Fatal("wrong kind")
	}
}
func TestAdmissionBinding(t *testing.T) {
	e := newEnv(1)
	defer e.close()
	base, _ := verify(e.admissionPub, e.admission, "admission-v1")
	for _, field := range []string{"ID", "Audience", "SPKI"} {
		c := base
		switch field {
		case "ID":
			c.ID = "other"
		case "Audience":
			c.Audience = "other"
		case "SPKI":
			c.SPKI = "other"
		}
		e.mu.Lock()
		e.admission = sign(e.admissionKey, c)
		accepted := e.admittedLocked()
		e.mu.Unlock()
		if accepted {
			t.Fatal(field + " accepted")
		}
	}
}
func TestCapture(t *testing.T) {
	c := capture{patterns: [][]byte{[]byte("secret")}, found: make([]bool, 1)}
	c.add([]byte("sec"))
	c.add([]byte("ret"))
	if !c.found[0] {
		t.Fatal("split secret missed")
	}
}

// Integration runs in Linux Docker, because macOS does not expose arbitrary 127/8 identities.
func TestLoopbackPrototype(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	for _, c := range negativeChecks() {
		if c.Status != "PASS" {
			t.Error(c.Name)
		}
	}
	for _, top := range []string{"A-O-B", "A-C-O-B", "A-O-C-B", "A-C-O-C-B"} {
		s, e := runTransfer(top, 1024*1024)
		if e != nil {
			t.Fatal(top, e)
		}
		if !validMeasurement(s) {
			t.Fatalf("%s violated conservation or secrecy", top)
		}
		want := 1
		if top == "A-O-B" {
			want = 0
		}
		if s.Unsafe {
			want = 2
		}
		if len(s.CommunityOrigins) != want {
			t.Fatalf("%s origins=%v", top, s.CommunityOrigins)
		}
	}
}

func TestCaptureInterleavedStreams(t *testing.T) {
	c := capture{patterns: [][]byte{[]byte("secret")}, found: make([]bool, 1)}
	c.stream("up", []byte("sec"))
	c.stream("down", []byte("unrelated"))
	c.stream("up", []byte("ret"))
	if !c.found[0] {
		t.Fatal("interleaved per-direction secret missed")
	}
}
func TestGrantCanaryIsLiteralInInnerPlaintext(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	s := sign(key, claims{Canary: canary, Hash: "full_hash_plaintext"})
	b, _ := json.Marshal(s)
	if !bytes.Contains(b, []byte(canary)) || !bytes.Contains(b, []byte("full_hash_plaintext")) {
		t.Fatal("scan would be meaningless: grant fields encoded before encryption")
	}
}
func TestMeasurementRejectsUncountedEgress(t *testing.T) {
	s := sample{Topology: "A-O-C-B", Bytes: 100, OperatorApplicationEgress: 100, OperatorTLSWireEgress: 110, OperatorOuterWireEgress: 110, CommunityTLSWireEgress: 115, CommunityDecryptedOuterBytes: 105, PayloadSHA256Verified: true, CommunitySecretScanClear: true, OperatorReadGrantsAndHash: true, CommunityOrigins: []string{bip}}
	if !validMeasurement(s) {
		t.Fatal("valid fixture refused")
	}
	for _, field := range []string{"operator", "outer", "community", "capture", "grant"} {
		bad := s
		switch field {
		case "operator":
			bad.OperatorTLSWireEgress = 0
		case "outer":
			bad.OperatorOuterWireEgress = 0
		case "community":
			bad.CommunityTLSWireEgress = 0
		case "capture":
			bad.CommunityDecryptedOuterBytes = 0
		case "grant":
			bad.OperatorReadGrantsAndHash = false
		}
		if validMeasurement(bad) {
			t.Fatal("uncounted source accepted: " + field)
		}
	}
}
func TestMedian(t *testing.T) {
	if median([]float64{3, 1, 2}) != 2 || median([]float64{4, 1, 2, 3}) != 2.5 {
		t.Fatal("median")
	}
}
