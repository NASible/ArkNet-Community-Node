package catalog

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"arknet/node-agent/internal/enrollment"
)

func TestFetchMediaMissesIsSignedAndStrict(t *testing.T) {
	id := "media_" + strings.Repeat("a", 64)
	for _, test := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", `{"misses":[{"media_id":"` + id + `","miss_count":2}]}`, true},
		{"empty", `{"misses":[]}`, true},
		{"null list", `{"misses":null}`, false},
		{"extra field", `{"misses":[{"media_id":"` + id + `","miss_count":2,"member_id":"x"}]}`, false},
		{"zero count", `{"misses":[{"media_id":"` + id + `","miss_count":0}]}`, false},
		{"bad id", `{"misses":[{"media_id":"../x","miss_count":1}]}`, false},
		{"duplicate", `{"misses":[{"media_id":"` + id + `","miss_count":1},{"media_id":"` + id + `","miss_count":1}]}`, false},
		{"trailing", `{"misses":[]}{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/federation/v1/nodes/"+catalogNodeID+"/media-misses" || r.Header.Get("X-ArkNet-Node-Signature") == "" {
					t.Errorf("unsigned or unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			seed := make([]byte, ed25519.SeedSize)
			misses, err := FetchMediaMisses(context.Background(), enrollment.Identity{NodeID: catalogNodeID, CoreURL: server.URL, Status: "APPROVED", PrivateSeed: seed})
			if test.ok != (err == nil) {
				t.Fatalf("misses=%+v err=%v", misses, err)
			}
		})
	}
}
