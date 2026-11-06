package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/store"
)

// capabilityNode is an approved, trusted Node identity holding capabilities.
type capabilityNode struct {
	nodes.Node
	key ed25519.PublicKey
}

func (c *capabilityNode) RegisterEnrollment(context.Context, string, string, []byte) (nodes.Node, error) {
	panic("not used")
}
func (c *capabilityNode) EnrollmentIdentity(_ context.Context, id string) (nodes.Node, []byte, error) {
	if id != c.PublicID {
		return nodes.Node{}, nil, nodes.ErrNotFound
	}
	return c.Node, c.key, nil
}

func newCapabilityNode(t *testing.T, capabilities ...nodes.Capability) (*capabilityNode, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &capabilityNode{Node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved,
		TrustStatus: nodes.TrustTrusted, Capabilities: capabilities}, key: pub}, priv
}

// downstream panics on any repository call; reached reports whether one happened.
type downstream struct {
	memberships.Repository
	store.MediaCatalogRepository
	store.MediaReplicaRepository
}

func serveNode(t *testing.T, h http.Handler, method, path, body string, key ed25519.PrivateKey) (code int, reached bool) {
	t.Helper()
	request := signedRequest(method, path, body, enrollmentID, key, strconv.FormatInt(time.Now().Unix(), 10))
	request.SetPathValue("node_id", enrollmentID)
	response := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() != nil {
				reached = true
			}
		}()
		h.ServeHTTP(response, request)
	}()
	return response.Code, reached
}

type nodeCapabilityRoute struct {
	name, method, path, body string
	required                 nodes.Capability
	handler                  func(identities nodes.HandshakeRepository) http.Handler
}

func nodeCapabilityRoutes(t *testing.T) []nodeCapabilityRoute {
	media := "media_" + strings.Repeat("c", 64)
	base := "/api/federation/v1/nodes/" + enrollmentID
	guard := func(pattern string, check func(http.ResponseWriter, *http.Request, nodes.HandshakeRepository) bool) func(nodes.HandshakeRepository) http.Handler {
		return func(identities nodes.HandshakeRepository) http.Handler {
			mux := http.NewServeMux()
			mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
				if check(w, r, identities) {
					panic("reached protected action")
				}
			})
			return mux
		}
	}
	return []nodeCapabilityRoute{
		{"invitations", "POST", base + "/invitations", policy("MEMBER", time.Hour, 1),
			nodes.CapabilityMemberships, func(i nodes.HandshakeRepository) http.Handler {
				mux := http.NewServeMux()
				mux.HandleFunc("POST /api/federation/v1/nodes/{node_id}/invitations", invitationCreateHandler(downstream{}, i))
				return mux
			}},
		{"offline authorizations", "POST", base + "/offline-authorizations", `{}`, nodes.CapabilityMemberships,
			guard("POST /api/federation/v1/nodes/{node_id}/offline-authorizations", func(w http.ResponseWriter, r *http.Request, i nodes.HandshakeRepository) bool {
				return authorizeOfflineNode(w, r, []byte(`{}`), i)
			})},
		{"tracker grants", "POST", base + "/media/" + media + "/tracker-grants", "", nodes.CapabilitySwarm,
			guard("POST /api/federation/v1/nodes/{node_id}/media/{id}/tracker-grants", func(w http.ResponseWriter, r *http.Request, i nodes.HandshakeRepository) bool {
				_, ok := authorizeTrackerGrantNode(w, r, i)
				return ok
			})},
		{"replica reports", "PUT", base + "/media/" + media + "/replicas", `{"size":1,"info_hash_v2":"` + strings.Repeat("d", 64) + `"}`,
			nodes.CapabilitySwarm, func(i nodes.HandshakeRepository) http.Handler {
				mux := http.NewServeMux()
				d := downstream{}
				mux.HandleFunc("PUT /api/federation/v1/nodes/{node_id}/media/{id}/replicas", mediaReplicaHandler(d, d, i))
				return mux
			}},
		// The real relay route: its "protected action" is signing the list, so
		// success (200) is the positive control instead of a panic.
		{"relay list", "GET", base + "/relays", "", nodes.CapabilitySwarm, func(i nodes.HandshakeRepository) http.Handler {
			relays := withRelays(http.NotFoundHandler(), testRelays(t), i, nil, nil, time.Now)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				recorder := httptest.NewRecorder()
				relays.ServeHTTP(recorder, r)
				if recorder.Code == http.StatusOK {
					panic("reached protected action")
				}
				w.WriteHeader(recorder.Code)
			})
		}},
	}
}

func TestNodeRoutesRequireTheirCapabilityAndDeniedCallsNeverReachRepositories(t *testing.T) {
	for _, route := range nodeCapabilityRoutes(t) {
		t.Run(route.name, func(t *testing.T) {
			var others []nodes.Capability
			for _, c := range nodes.AllCapabilities() {
				if c != route.required {
					others = append(others, c)
				}
			}
			for _, held := range [][]nodes.Capability{nil, others} {
				identity, key := newCapabilityNode(t, held...)
				code, reached := serveNode(t, route.handler(identity), route.method, route.path, route.body, key)
				if code != http.StatusForbidden || reached {
					t.Fatalf("holding %v: status %d reached %v", held, code, reached)
				}
			}
			// Positive control: with the capability the request passes the guard.
			identity, key := newCapabilityNode(t, route.required)
			if code, reached := serveNode(t, route.handler(identity), route.method, route.path, route.body, key); !reached {
				t.Fatalf("granted: status %d, never reached the protected action", code)
			}
		})
	}
}

func TestBaseNodeRoutesNeedNoCapability(t *testing.T) {
	identity, key := newCapabilityNode(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/media", func(w http.ResponseWriter, r *http.Request) {
		if authorizeMediaNode(w, r, identity) {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	if code, _ := serveNode(t, mux, "GET", "/api/federation/v1/nodes/"+enrollmentID+"/media", "", key); code != http.StatusNoContent {
		t.Fatalf("catalog read without capabilities: %d", code)
	}
}

func TestNodeCapabilitiesAreAClosedSet(t *testing.T) {
	if len(nodes.AllCapabilities()) != 2 {
		t.Fatalf("capabilities = %v", nodes.AllCapabilities())
	}
	n := nodes.Node{Capabilities: []nodes.Capability{nodes.CapabilitySwarm}}
	if !n.Can(nodes.CapabilitySwarm) || n.Can(nodes.CapabilityMemberships) || n.Can("") {
		t.Fatal("Can is wrong")
	}
	for _, bad := range [][]string{{"root"}, {"swarm", "swarm"}, {"SWARM"}} {
		if _, err := nodes.NormalizeCapabilities(bad); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	if got, err := nodes.NormalizeCapabilities([]string{"swarm", "memberships"}); err != nil || len(got) != 2 || got[0] != nodes.CapabilityMemberships {
		t.Fatalf("normalize: %v %v", got, err)
	}
	if got, err := nodes.NormalizeCapabilities([]string{}); err != nil || len(got) != 0 {
		t.Fatalf("empty set (base only) rejected: %v", err)
	}
}
