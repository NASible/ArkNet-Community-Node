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

func TestResponseRequiresCompleteEnvelope(t *testing.T) {
	b, _ := json.Marshal(goodStatus())
	var fields map[string]any
	json.Unmarshal(b, &fields)
	for _, field := range []string{"object_id", "state", "total_bytes", "completed_bytes", "mock", "error_code"} {
		t.Run("missing "+field, func(t *testing.T) {
			copy := map[string]any{}
			for k, v := range fields {
				if k != field {
					copy[k] = v
				}
			}
			payload, _ := json.Marshal(copy)
			s := jsonServer(string(payload))
			defer s.Close()
			_, err := New(s.URL, testToken).Get(context.Background(), testID)
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("accepted missing field %v", err)
			}
		})
	}
	for _, body := range []string{`null`, `{}`, `{"transfers":null}`, `{"transfers":[],"transfers":[]}`} {
		t.Run(body, func(t *testing.T) {
			s := jsonServer(body)
			defer s.Close()
			_, err := New(s.URL, testToken).List(context.Background())
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("accepted malformed list %v", err)
			}
		})
	}
}
func jsonServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
}
func TestWrongTokenRejected(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer "+testToken {
			t.Error("expected wrong token")
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer s.Close()
	_, err := New(s.URL, strings.Repeat("b", 32)).Get(context.Background(), testID)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("%v", err)
	}
}
func TestTransportDoesNotUseProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	s := jsonServer(mustStatusJSON())
	defer s.Close()
	c := New(s.URL+"/", testToken)
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("proxy enabled")
	}
	if _, err := c.Get(context.Background(), testID); err != nil {
		t.Fatal(err)
	}
}
func mustStatusJSON() string { b, _ := json.Marshal(goodStatus()); return string(b) }
func TestAllValidStates(t *testing.T) {
	for _, state := range []State{Downloading, Paused, Completed, Cancelled, VerificationFailed} {
		t.Run(string(state), func(t *testing.T) {
			status := goodStatus()
			status.State = state
			if state == Completed {
				status.CompletedBytes = status.TotalBytes
			}
			if state == VerificationFailed {
				status.ErrorCode = "verification_failed"
			}
			b, _ := json.Marshal(status)
			s := jsonServer(string(b))
			defer s.Close()
			got, err := New(s.URL, testToken).Get(context.Background(), testID)
			if err != nil || got != status {
				t.Fatalf("%#v %v", got, err)
			}
		})
	}
}
