package transfer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResourceRoutes(t *testing.T) {
	for _, action := range []string{"get", "pause", "resume", "cancel", "list"} {
		t.Run(action, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := "/api/transfer/v1/transfers"
				method := "GET"
				if action != "list" {
					path += "/" + testID
				}
				if action != "list" && action != "get" {
					path += "/" + action
					method = "POST"
					var body map[string]any
					if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 0 {
						t.Error("action body must be {}")
					}
				}
				if r.Method != method || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer "+testToken {
					t.Error("wire contract")
				}
				w.Header().Set("Content-Type", "application/json")
				if action == "list" {
					json.NewEncoder(w).Encode(map[string]any{"transfers": []Status{goodStatus()}})
				} else {
					json.NewEncoder(w).Encode(goodStatus())
				}
			}))
			defer s.Close()
			c := New(s.URL, testToken)
			ctx := context.Background()
			var got Status
			var err error
			switch action {
			case "get":
				got, err = c.Get(ctx, testID)
			case "pause":
				got, err = c.Pause(ctx, testID)
			case "resume":
				got, err = c.Resume(ctx, testID)
			case "cancel":
				got, err = c.Cancel(ctx, testID)
			case "list":
				var all []Status
				all, err = c.List(ctx)
				if len(all) != 1 {
					t.Fatalf("list %#v %v", all, err)
				}
				got = all[0]
			}
			if err != nil || got != goodStatus() {
				t.Fatalf("got %#v %v", got, err)
			}
		})
	}
}
