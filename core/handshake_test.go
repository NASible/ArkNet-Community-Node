package main

import (
	"arknet/core/internal/nodes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type handshakeRepo struct {
	node   nodes.Node
	key    []byte
	writes int
	err    error
}

func (s *handshakeRepo) Enroll(context.Context, string, string) (nodes.Node, error) {
	return s.node, s.err
}
func (s *handshakeRepo) RegisterEnrollment(_ context.Context, id, v string, key []byte) (nodes.Node, error) {
	s.writes++
	s.key = append([]byte(nil), key...)
	s.node = nodes.Node{PublicID: id, SoftwareVersion: v, EnrollmentStatus: nodes.EnrollmentPending, TrustStatus: nodes.TrustUntrusted}
	return s.node, s.err
}
func (s *handshakeRepo) EnrollmentIdentity(context.Context, string) (nodes.Node, []byte, error) {
	return s.node, s.key, s.err
}
func signedRequest(method, path, body, id string, key ed25519.PrivateKey, timestamp string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-ArkNet-Node-Timestamp", timestamp)
	message := fmt.Sprintf("ARKNET-NODE-V1\n%s\n%s\n%x\n%s\n%s", method, r.URL.EscapedPath(), sha256.Sum256([]byte(body)), timestamp, id)
	r.Header.Set("X-ArkNet-Node-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(message))))
	return r
}
func TestSignedRegistrationAndStatus(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	repo := &handshakeRepo{}
	body := fmt.Sprintf(`{"node_id":%q,"version":"v","capabilities":{},"public_key":%q}`, enrollmentID, base64.StdEncoding.EncodeToString(pub))
	h := newHandler(nil, repo)
	ts := fmt.Sprint(time.Now().Unix())
	r := signedRequest("POST", enrollmentPath, body, enrollmentID, priv, ts)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || repo.writes != 1 {
		t.Fatalf("registration: %d %s writes=%d", w.Code, w.Body, repo.writes)
	}
	var receipt map[string]any
	if json.Unmarshal(w.Body.Bytes(), &receipt) != nil || len(receipt) != 2 || receipt["node_id"] != enrollmentID || receipt["enrollment_status"] != "PENDING" {
		t.Fatalf("signed receipt must match Agent's exact two-field contract: %s", w.Body)
	}
	repo.node.EnrollmentStatus = nodes.EnrollmentApproved
	repo.node.TrustStatus = nodes.TrustTrusted
	r = signedRequest("GET", "/api/federation/v1/nodes/"+enrollmentID+"/enrollment", "", enrollmentID, priv, ts)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != 200 || len(got) != 3 || got["node_id"] != enrollmentID || got["enrollment_status"] != "APPROVED" || got["trust_status"] != "TRUSTED" {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
}

func TestHandshakeRejectsInvalidProofBeforeStorage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	body := fmt.Sprintf(`{"node_id":%q,"version":"v","capabilities":{},"public_key":%q}`, enrollmentID, base64.StdEncoding.EncodeToString(pub))
	ts := fmt.Sprint(time.Now().Unix())
	cases := []struct {
		name   string
		change func(*http.Request)
	}{
		{"missing timestamp", func(r *http.Request) { r.Header.Del("X-ArkNet-Node-Timestamp") }},
		{"missing signature", func(r *http.Request) { r.Header.Del("X-ArkNet-Node-Signature") }},
		{"duplicate timestamp", func(r *http.Request) { r.Header.Add("X-ArkNet-Node-Timestamp", ts) }},
		{"duplicate signature", func(r *http.Request) {
			r.Header.Add("X-ArkNet-Node-Signature", r.Header.Get("X-ArkNet-Node-Signature"))
		}},
		{"timestamp leading zero", func(r *http.Request) { r.Header.Set("X-ArkNet-Node-Timestamp", "0"+ts) }},
		{"expired", func(r *http.Request) { r.Header.Set("X-ArkNet-Node-Timestamp", fmt.Sprint(time.Now().Unix()-61)) }},
		{"future", func(r *http.Request) { r.Header.Set("X-ArkNet-Node-Timestamp", fmt.Sprint(time.Now().Unix()+61)) }},
		{"query", func(r *http.Request) { r.URL.RawQuery = "extra=1" }},
		{"force query", func(r *http.Request) { r.URL.ForceQuery = true }},
		{"wrong key", func(r *http.Request) {
			r.Header = signedRequest("POST", enrollmentPath, body, enrollmentID, wrong, ts).Header
		}},
		{"wrong id", func(r *http.Request) {
			r.Header = signedRequest("POST", enrollmentPath, body, "node_"+strings.Repeat("b", 64), priv, ts).Header
		}},
		{"tampered body", func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader(strings.Replace(body, "\"v\"", "\"w\"", 1)))
		}},
		{"noncanonical signature", func(r *http.Request) {
			r.Header.Set("X-ArkNet-Node-Signature", r.Header.Get("X-ArkNet-Node-Signature")+"\n")
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			repo := &handshakeRepo{}
			r := signedRequest("POST", enrollmentPath, body, enrollmentID, priv, ts)
			tt.change(r)
			w := httptest.NewRecorder()
			newHandler(nil, repo).ServeHTTP(w, r)
			if w.Code != 401 || repo.writes != 0 {
				t.Fatalf("%d %s writes=%d", w.Code, w.Body, repo.writes)
			}
		})
	}
}
func TestSignedRegistrationRejectsDuplicateJSONKeys(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, extra := range []string{`"node_id":"` + enrollmentID + `",`, `"version":"v",`, `"capabilities":{},`, `"public_key":"` + base64.StdEncoding.EncodeToString(pub) + `",`} {
		body := fmt.Sprintf(`{%s"node_id":%q,"version":"v","capabilities":{},"public_key":%q}`, extra, enrollmentID, base64.StdEncoding.EncodeToString(pub))
		repo := &handshakeRepo{}
		r := signedRequest("POST", enrollmentPath, body, enrollmentID, priv, fmt.Sprint(time.Now().Unix()))
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, r)
		if w.Code != 400 || repo.writes != 0 {
			t.Fatalf("duplicate accepted: %d %s", w.Code, w.Body)
		}
	}
}

func TestStatusProofIsBoundToStoredKeyAndTarget(t *testing.T) {
	pubA, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)
	idB := "node_" + strings.Repeat("b", 64)
	path := "/api/federation/v1/nodes/" + idB + "/enrollment"
	ts := fmt.Sprint(time.Now().Unix())
	base := nodes.Node{PublicID: idB, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}
	tests := []struct {
		name, method, body, id string
		key                    ed25519.PrivateKey
		stored                 []byte
		node                   nodes.Node
		err                    error
		status                 int
		query                  string
	}{
		{"B valid", "GET", "", idB, privB, pubB, base, nil, 200, ""},
		{"A claims B", "GET", "", idB, privA, pubB, base, nil, 401, ""},
		{"A id signature", "GET", "", enrollmentID, privA, pubA, base, nil, 401, ""},
		{"legacy approved unbound", "GET", "", idB, privB, nil, base, nil, 401, ""},
		{"unknown", "GET", "", idB, privB, nil, nodes.Node{}, nodes.ErrNotFound, 401, ""},
		{"database failure", "GET", "", idB, privB, nil, nodes.Node{}, errors.New("secret database failure"), 503, ""},
		{"body", "GET", "x", idB, privB, pubB, base, nil, 401, ""},
		{"query", "GET", "", idB, privB, pubB, base, nil, 401, "?x=y"},
		{"empty query", "GET", "", idB, privB, pubB, base, nil, 401, "?"},
		{"head not GET", "HEAD", "", idB, privB, pubB, base, nil, 401, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &handshakeRepo{node: tt.node, key: tt.stored, err: tt.err}
			w := httptest.NewRecorder()
			newHandler(nil, repo).ServeHTTP(w, signedRequest(tt.method, path+tt.query, tt.body, tt.id, tt.key, ts))
			if w.Code != tt.status || strings.Contains(w.Body.String(), "secret") {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if repo.writes != 0 {
				t.Fatal("GET wrote state")
			}
		})
	}
	for _, pair := range [][2]string{{"PENDING", "UNTRUSTED"}, {"REJECTED", "BLOCKED"}, {"APPROVED", "UNTRUSTED"}, {"other", "TRUSTED"}} {
		repo := &handshakeRepo{node: base, key: pubB}
		repo.node.EnrollmentStatus = nodes.EnrollmentStatus(pair[0])
		repo.node.TrustStatus = nodes.TrustStatus(pair[1])
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("GET", path, "", idB, privB, ts))
		want := 200
		if pair[0] == "other" || pair[0] == "APPROVED" {
			want = 503
		}
		if w.Code != want {
			t.Fatalf("pair %v: %d", pair, w.Code)
		}
	}
}

func TestSignedEnrollmentExactWireFieldsAndKeyEncoding(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	original := fmt.Sprintf(`{"node_id":%q,"version":"v","capabilities":{},"public_key":%q}`, enrollmentID, base64.StdEncoding.EncodeToString(pub))
	bodies := []string{
		strings.TrimSuffix(original, "}") + `,"extra":true}`,
		strings.Replace(original, `"public_key"`, `"PUBLIC_KEY"`, 1),
		strings.Replace(original, base64.StdEncoding.EncodeToString(pub), base64.RawStdEncoding.EncodeToString(pub), 1),
		strings.Replace(original, base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(pub[:31]), 1),
		strings.Replace(original, `"capabilities":{}`, `"capabilities":null`, 1),
		strings.Replace(original, `"capabilities":{}`, `"capabilities":{"extra":true}`, 1),
	}
	for _, body := range bodies {
		repo := &handshakeRepo{}
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", enrollmentPath, body, enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
		if w.Code != 400 || repo.writes != 0 {
			t.Fatalf("invalid body accepted: %d %s", w.Code, w.Body)
		}
	}
}
func TestSignedEnrollmentRepositoryErrorsAreSanitized(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := fmt.Sprintf(`{"node_id":%q,"version":"v","capabilities":{},"public_key":%q}`, enrollmentID, base64.StdEncoding.EncodeToString(pub))
	for _, tt := range []struct {
		err    error
		status int
	}{{fmt.Errorf("secret: %w", nodes.ErrEnrollmentConflict), 200}, {errors.New("secret database failure"), 503}} {
		repo := &handshakeRepo{err: tt.err}
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", enrollmentPath, body, enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
		if w.Code != tt.status || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}
