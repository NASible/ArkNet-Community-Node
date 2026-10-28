package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectInvalidOutgoingRequest(t *testing.T) {
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(goodStatus())
	}))
	defer s.Close()
	c := New(s.URL, testToken)
	for _, r := range []Request{{ObjectID: "../secret", Size: 10}, {ObjectID: "opaque-1", Size: 10}, {ObjectID: testID, Size: -1}, {ObjectID: testID, Size: 10, InfoHashV2: "https://secret"}, {ObjectID: testID, Size: 10, InfoHashV2: strings.Repeat("A", 64)}} {
		_, err := c.Add(context.Background(), r)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("request accepted: %v", err)
		}
	}
	for _, id := range []string{"", "../secret", "/", "a?token=secret", "media_"} {
		_, err := c.Get(context.Background(), id)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ID accepted: %v", err)
		}
	}
	if hits != 0 {
		t.Errorf("%d invalid requests reached service", hits)
	}
}
func TestValidateReturnedStatus(t *testing.T) {
	cases := map[string]func(*Status){"negative total": func(s *Status) { s.TotalBytes = -1 }, "negative completed": func(s *Status) { s.CompletedBytes = -1 }, "overflow": func(s *Status) { s.CompletedBytes = 11 }, "invalid id": func(s *Status) { s.ObjectID = "../secret" }, "mismatched id": func(s *Status) { s.ObjectID = "media_" + strings.Repeat("b", 64) }, "unknown state": func(s *Status) { s.State = "UNKNOWN" }, "unexpected code": func(s *Status) { s.ErrorCode = "secret" }, "missing failure code": func(s *Status) { s.State = VerificationFailed }, "incomplete completed": func(s *Status) { s.State = Completed }}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			result := goodStatus()
			mutate(&result)
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(result)
			}))
			defer s.Close()
			got, err := New(s.URL, testToken).Get(context.Background(), testID)
			if !errors.Is(err, ErrInvalidResponse) || got != (Status{}) {
				t.Fatalf("got %#v %v", got, err)
			}
		})
	}
}
func TestAddRejectsSizeMismatch(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(goodStatus())
	}))
	defer s.Close()
	got, err := New(s.URL, testToken).Add(context.Background(), Request{ObjectID: testID, Size: 11, InfoHashV2: strings.Repeat("a", 64), TrackerURL: "https://tracker.example/announce/grant", WebSeedURL: "https://core.example/api/web-seed/v1/media/" + testID + "/grant", TorrentURL: "https://core.example/api/torrent/v1/media/" + testID + "/grant"})
	if !errors.Is(err, ErrInvalidResponse) || got != (Status{}) {
		t.Fatalf("got %#v %v", got, err)
	}
}
