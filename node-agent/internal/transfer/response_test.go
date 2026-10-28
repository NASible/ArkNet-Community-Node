package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectInvalidJSONResponses(t *testing.T) {
	b, _ := json.Marshal(goodStatus())
	valid := string(b)
	cases := map[string]struct{ body, ct string }{
		"malformed": {"{", "application/json"}, "trailing": {valid + " {}", "application/json"}, "unknown": {strings.TrimSuffix(valid, "}") + `,"secret":"private"}`, "application/json"}, "wrong type": {valid, "text/plain"}, "missing type": {valid, ""}, "null": {"null", "application/json"}, "oversize": {valid + strings.Repeat(" ", 1<<20), "application/json"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.ct != "" {
					w.Header().Set("Content-Type", tc.ct)
				}
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			got, err := New(s.URL, testToken).Get(context.Background(), testID)
			if !errors.Is(err, ErrInvalidResponse) || got != (Status{}) {
				t.Fatalf("got %#v %v", got, err)
			}
		})
	}
}
func TestListRejectsInvalidOrDuplicateEntries(t *testing.T) {
	good := goodStatus()
	bad := good
	bad.TotalBytes = -1
	for name, entries := range map[string][]Status{"duplicate": {good, good}, "invalid": {bad}} {
		t.Run(name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"transfers": entries})
			}))
			defer s.Close()
			got, err := New(s.URL, testToken).List(context.Background())
			if !errors.Is(err, ErrInvalidResponse) || got != nil {
				t.Fatalf("got %#v %v", got, err)
			}
		})
	}
}
