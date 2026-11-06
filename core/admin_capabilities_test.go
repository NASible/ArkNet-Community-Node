package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"arknet/core/internal/admins"
	"arknet/core/internal/nodes"
)

func TestAdminSetsNodeCapabilitiesWithAnExactBody(t *testing.T) {
	path := "/api/admin/v1/nodes/" + enrollmentID + "/capabilities"
	for _, test := range []struct {
		body, want string
	}{
		{`{"capabilities":["swarm","memberships"]}`, "memberships,swarm"},
		{`{"capabilities":["swarm"]}`, "swarm"},
		{`{"capabilities":[]}`, ""},
	} {
		spy, auth := &spyRepositories{}, newFakeAdmins()
		h := withAdministration(newHandler(healthyDatabase{}), spy, auth)
		response := serveAdmin(h, adminRoute{method: "PUT", path: path, body: test.body}, auth.grant(admins.NodesDecide))
		var got struct {
			NodeID       string   `json:"node_id"`
			Capabilities []string `json:"capabilities"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil || got.NodeID != enrollmentID ||
			got.Capabilities == nil || strings.Join(got.Capabilities, ",") != test.want {
			t.Fatalf("%s: %d %s", test.body, response.Code, response.Body)
		}
	}
	for _, body := range []string{"", `{}`, `{"capabilities":["root"]}`, `{"capabilities":["swarm","swarm"]}`,
		`{"capabilities":"swarm"}`, `{"capabilities":["swarm"],"node_id":"x"}`, `{"capabilities":["swarm"]} {}`, `{"capabilities":null}`} {
		spy, auth := &spyRepositories{}, newFakeAdmins()
		h := withAdministration(newHandler(healthyDatabase{}), spy, auth)
		if response := serveAdmin(h, adminRoute{method: "PUT", path: path, body: body}, auth.grant(admins.NodesDecide)); response.Code != 400 || spy.count() != 0 {
			t.Fatalf("%q: %d calls %v", body, response.Code, spy.calls)
		}
	}
	spy, auth := &spyRepositories{}, newFakeAdmins()
	h := withAdministration(newHandler(healthyDatabase{}), spy, auth)
	if response := serveAdmin(h, adminRoute{method: "PUT", path: path + "?x=1", body: `{"capabilities":[]}`}, auth.grant(admins.NodesDecide)); response.Code != 400 || spy.count() != 0 {
		t.Fatalf("query accepted: %d", response.Code)
	}
}

func TestAdminNodeMetadataShowsCapabilities(t *testing.T) {
	repo := &metadataRepo{node: nodes.Node{PublicID: enrollmentID, EnrollmentStatus: nodes.EnrollmentApproved,
		TrustStatus: nodes.TrustTrusted, Capabilities: []nodes.Capability{nodes.CapabilitySwarm}}}
	w := httptest.NewRecorder()
	withAdministration(newHandler(nil), repo, allowAdmin(true)).ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/v1/nodes/"+enrollmentID, nil))
	var got map[string]json.RawMessage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || string(got["capabilities"]) != `["swarm"]` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	repo.node.Capabilities = nil
	w = httptest.NewRecorder()
	withAdministration(newHandler(nil), repo, allowAdmin(true)).ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/v1/nodes/"+enrollmentID, nil))
	if json.Unmarshal(w.Body.Bytes(), &got) != nil || string(got["capabilities"]) != `[]` {
		t.Fatalf("empty capabilities rendered as %s", got["capabilities"])
	}
}
