package main

import (
	"arknet/core/internal/nodes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type heartbeatRepo struct {
	handshakeRepo
	proof                    int64
	calls                    int
	gotVersion, gotState     string
	healthy, databaseHealthy bool
	peerServing              bool
	mutationErr              error
}

func (s *heartbeatRepo) AcceptHeartbeat(_ context.Context, id, version, state string, healthy, databaseHealthy, peerServing bool, key []byte, proof int64) error {
	s.calls++
	s.proof = proof
	s.gotVersion = version
	s.gotState = state
	s.healthy = healthy
	s.databaseHealthy = databaseHealthy
	s.peerServing = peerServing
	return s.mutationErr
}
func heartbeatBody() string {
	return fmt.Sprintf(`{"node_id":%q,"version":"v2","state":"CONNECTED","health":{"healthy":true,"database_healthy":false},"capabilities":{"peer_serving":true}}`, enrollmentID)
}

const heartbeatPrefix = "/api/federation/v1/nodes/"

func TestHeartbeatAuthenticationAndEligibility(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	approved := nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}
	for _, tt := range []struct {
		name   string
		node   nodes.Node
		key    []byte
		err    error
		signer ed25519.PrivateKey
		change func(*http.Request)
		want   int
	}{
		{"unknown", nodes.Node{}, nil, nodes.ErrNotFound, priv, nil, 401},
		{"database", nodes.Node{}, nil, errors.New("private connection password"), priv, nil, 503},
		{"unbound", approved, nil, nil, priv, nil, 401},
		{"wrong key", approved, pub, nil, wrong, nil, 401},
		{"wrong signature id", approved, pub, nil, priv, func(r *http.Request) {
			r.Header = signedRequest("POST", r.URL.Path, heartbeatBody(), "node_"+strings.Repeat("b", 64), priv, fmt.Sprint(time.Now().Unix())).Header
		}, 401},
		{"missing signature", approved, pub, nil, priv, func(r *http.Request) { r.Header.Del("X-ArkNet-Node-Signature") }, 401},
		{"duplicate timestamp", approved, pub, nil, priv, func(r *http.Request) {
			r.Header.Add("X-ArkNet-Node-Timestamp", r.Header.Get("X-ArkNet-Node-Timestamp"))
		}, 401},
		{"noncanonical", approved, pub, nil, priv, func(r *http.Request) {
			r.Header.Set("X-ArkNet-Node-Timestamp", "0"+r.Header.Get("X-ArkNet-Node-Timestamp"))
		}, 401},
		{"old GET proof", approved, pub, nil, priv, func(r *http.Request) {
			r.Header = signedRequest("GET", heartbeatPrefix+enrollmentID+"/enrollment", "", enrollmentID, priv, fmt.Sprint(time.Now().Unix())).Header
		}, 401},
		{"old enrollment proof", approved, pub, nil, priv, func(r *http.Request) {
			r.Header = signedRequest("POST", enrollmentPath, heartbeatBody(), enrollmentID, priv, fmt.Sprint(time.Now().Unix())).Header
		}, 401},
		{"pending", nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentPending, TrustStatus: nodes.TrustUntrusted}, pub, nil, priv, nil, 403},
		{"rejected", nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentRejected, TrustStatus: nodes.TrustBlocked}, pub, nil, priv, nil, 403},
		{"not trusted", nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustUntrusted}, pub, nil, priv, nil, 403},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &heartbeatRepo{handshakeRepo: handshakeRepo{node: tt.node, key: tt.key, err: tt.err}}
			r := signedRequest("POST", heartbeatPrefix+enrollmentID+"/heartbeats", heartbeatBody(), enrollmentID, tt.signer, fmt.Sprint(time.Now().Unix()))
			if tt.change != nil {
				tt.change(r)
			}
			w := httptest.NewRecorder()
			newHandler(nil, repo).ServeHTTP(w, r)
			if w.Code != tt.want || repo.calls != 0 || strings.Contains(w.Body.String(), "password") {
				t.Fatalf("%d %s calls %d", w.Code, w.Body, repo.calls)
			}
		})
	}
}

func TestHeartbeatMutationErrors(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, tt := range []struct {
		err    error
		status int
	}{{nodes.ErrHeartbeatReplay, 409}, {nodes.ErrNodeAuthentication, 401}, {nodes.ErrNotFound, 401}, {nodes.ErrNodeUnapproved, 403}, {errors.New("secret database failure"), 503}} {
		repo := &heartbeatRepo{handshakeRepo: handshakeRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, key: pub}, mutationErr: fmt.Errorf("secret: %w", tt.err)}
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", heartbeatPrefix+enrollmentID+"/heartbeats", heartbeatBody(), enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
		if w.Code != tt.status || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
}

func TestHeartbeatAcceptedExactReceipt(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	repo := &heartbeatRepo{handshakeRepo: handshakeRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, key: pub}}
	ts := time.Now().Unix()
	w := httptest.NewRecorder()
	newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", heartbeatPrefix+enrollmentID+"/heartbeats", heartbeatBody(), enrollmentID, priv, fmt.Sprint(ts)))
	var receipt map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil || len(receipt) != 2 || receipt["node_id"] != enrollmentID || receipt["accepted"] != true || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("receipt: %d %s", w.Code, w.Body)
	}
	if repo.calls != 1 || repo.proof != ts || repo.gotVersion != "v2" || repo.gotState != "CONNECTED" || !repo.healthy || repo.databaseHealthy || !repo.peerServing {
		t.Fatalf("mutation: %+v", repo)
	}
}

func TestHeartbeatStrictPayload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	base := heartbeatBody()
	bodies := []string{"null", "[]", base + "{}", strings.Replace(base, `"version":"v2"`, `"version":null`, 1), strings.Replace(base, `"version":"v2"`, `"version":""`, 1), strings.Replace(base, `"state":"CONNECTED"`, `"state":"connected"`, 1), strings.Replace(base, `"state":"CONNECTED"`, `"state":"operator@example.com"`, 1), strings.Replace(base, `"node_id"`, `"NODE_ID"`, 1), strings.Replace(base, enrollmentID, "node_"+strings.Repeat("b", 64), 1), strings.Replace(base, `"capabilities":{"peer_serving":true}`, `"capabilities":null`, 1), strings.Replace(base, `"capabilities":{"peer_serving":true}`, `"capabilities":{}`, 1), strings.Replace(base, `"capabilities":{"peer_serving":true}`, `"capabilities":{"peer_serving":"true"}`, 1), strings.Replace(base, `"capabilities":{"peer_serving":true}`, `"capabilities":{"peer_serving":true,"private":"x"}`, 1), strings.Replace(base, `"health":{"healthy":true,"database_healthy":false}`, `"health":{}`, 1), strings.Replace(base, `"healthy":true`, `"healthy":null`, 1), strings.Replace(base, `"healthy":true`, `"healthy":"true"`, 1), strings.Replace(base, `"healthy":true`, `"HEALTHY":true`, 1), strings.Replace(base, `"healthy":true`, `"healthy":true,"private":"x"`, 1), strings.Replace(base, `"healthy":true`, `"healthy":true,"healthy":false`, 1), strings.TrimSuffix(base, "}") + `,"unknown":true}`}
	for _, name := range []string{"node_id", "version", "state", "health", "capabilities"} {
		var fields map[string]json.RawMessage
		json.Unmarshal([]byte(base), &fields)
		bodies = append(bodies, strings.TrimSuffix(base, "}")+fmt.Sprintf(`,%q:%s}`, name, fields[name]))
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			repo := &heartbeatRepo{handshakeRepo: handshakeRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, key: pub}}
			w := httptest.NewRecorder()
			newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", heartbeatPrefix+enrollmentID+"/heartbeats", body, enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
			if w.Code != 400 || repo.calls != 0 {
				t.Fatalf("invalid accepted: %d %s calls=%d", w.Code, w.Body, repo.calls)
			}
		})
	}
	for _, state := range []string{"PROVISIONING", "ENROLLMENT_PENDING", "CONNECTED", "SYNCHRONIZING", "MESH", "DEGRADED", "OFFLINE", "RECONNECTING", "QUARANTINED", "REVOKED", "ERROR"} {
		repo := &heartbeatRepo{handshakeRepo: handshakeRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, key: pub}}
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", heartbeatPrefix+enrollmentID+"/heartbeats", strings.Replace(base, "CONNECTED", state, 1), enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
		if w.Code != 200 {
			t.Fatalf("state %s: %d", state, w.Code)
		}
	}
}
func TestHeartbeatRejectsOversizeAndQuery(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, tt := range []struct {
		body, suffix string
		status       int
	}{{heartbeatBody() + strings.Repeat(" ", 4096), "", 413}, {heartbeatBody(), "?x=1", 400}, {heartbeatBody(), "?", 400}} {
		repo := &heartbeatRepo{handshakeRepo: handshakeRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved, TrustStatus: nodes.TrustTrusted}, key: pub}}
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, signedRequest("POST", heartbeatPrefix+enrollmentID+"/heartbeats"+tt.suffix, tt.body, enrollmentID, priv, fmt.Sprint(time.Now().Unix())))
		if w.Code != tt.status || repo.calls != 0 {
			t.Fatalf("got %d want %d", w.Code, tt.status)
		}
	}
}
