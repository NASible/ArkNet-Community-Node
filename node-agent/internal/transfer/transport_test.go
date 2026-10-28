package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRedirectBlocked(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(goodStatus())
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/secret", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	got, err := New(source.URL, testToken).Get(context.Background(), testID)
	if err == nil || got != (Status{}) || hits.Load() != 0 {
		t.Fatalf("redirect followed: %#v %v hits=%d", got, err, hits.Load())
	}
	if strings.Contains(err.Error(), source.URL) || strings.Contains(err.Error(), testToken) {
		t.Fatal("private error")
	}
}
func TestNetworkErrorsSanitized(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	s.Close()
	_, err := New(s.URL, testToken).Get(context.Background(), testID)
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), s.URL) || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unsafe network error %v", err)
	}
}
func TestCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := New(s.URL, testToken).Get(ctx, testID); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation ignored")
	}
}
func TestResponseHeaderDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(4 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(goodStatus())
		}
	}))
	defer s.Close()
	start := time.Now()
	_, err := New(s.URL, testToken).Get(context.Background(), testID)
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 3800*time.Millisecond {
		t.Fatalf("header deadline %s %v", time.Since(start), err)
	}
}
func TestTotalDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(6 * time.Second):
			json.NewEncoder(w).Encode(goodStatus())
		}
	}))
	defer s.Close()
	start := time.Now()
	_, err := New(s.URL, testToken).Get(context.Background(), testID)
	if !errors.Is(err, ErrUnavailable) || time.Since(start) > 5800*time.Millisecond {
		t.Fatalf("total deadline %s %v", time.Since(start), err)
	}
}
