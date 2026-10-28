package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestEndpoints(t *testing.T) {
	tests := []struct {
		path string
		want map[string]any
	}{
		{"/health", map[string]any{"healthy": true, "database_healthy": true}},
		{"/api/federation/v1/version", map[string]any{"version": "0.0.1-dev"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			newHandler(healthyDatabase{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			if got := response.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q", got)
			}
			var got map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("unexpected JSON: %v", got)
			}
			for key, value := range tt.want {
				if got[key] != value {
					t.Errorf("%s = %v, want %v", key, got[key], value)
				}
			}
		})
	}
}

func TestEndpointsRejectPost(t *testing.T) {
	for _, path := range []string{"/health", "/api/federation/v1/version"} {
		response := httptest.NewRecorder()
		newHandler(healthyDatabase{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d", path, response.Code)
		}
	}
}

func TestUnknownRoute(t *testing.T) {
	response := httptest.NewRecorder()
	newHandler(healthyDatabase{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestVersionMatchesStack(t *testing.T) {
	data, err := os.ReadFile("../VERSION")
	if err != nil {
		t.Fatal(err)
	}
	if version != strings.TrimSpace(string(data)) {
		t.Fatalf("service version %q differs from VERSION", version)
	}
}

type healthyDatabase struct{}

func (healthyDatabase) Ping(context.Context) error { return nil }

type pingFunc func(context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestHealthReportsDatabaseFailure(t *testing.T) {
	for _, db := range []databaseHealth{nil, pingFunc(func(context.Context) error { return errors.New("secret database error") })} {
		response := httptest.NewRecorder()
		newHandler(db).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", response.Code)
		}
		if response.Header().Get("Content-Type") != "application/json" {
			t.Fatal("health must be JSON")
		}
		var got map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["healthy"] != false || got["database_healthy"] != false || len(got) != 2 {
			t.Fatalf("unexpected response: %v", got)
		}
		if strings.Contains(response.Body.String(), "secret") {
			t.Fatal("database details leaked")
		}
	}
}

func TestHealthPingIsBoundedAndRequestCancellationPropagates(t *testing.T) {
	db := pingFunc(func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Fatal("health ping has no bounded timeout")
		}
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	newHandler(db).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}
