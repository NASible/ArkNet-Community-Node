package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"arknet/node-agent/internal/application"
)

type lifecycleRecorder struct{ operation, applicationID string }

func (l *lifecycleRecorder) Start(_ context.Context, id string) error {
	l.operation, l.applicationID = "start", id
	return nil
}
func (l *lifecycleRecorder) Stop(_ context.Context, id string) error {
	l.operation, l.applicationID = "stop", id
	return nil
}
func (l *lifecycleRecorder) Restart(_ context.Context, id string) error {
	l.operation, l.applicationID = "restart", id
	return nil
}

func TestAdapterReportsHealthConfigurationVersionAndEnabledState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte("Healthy"))
		case "/System/Info/Public":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Version":"10.10.7"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := New(server.URL, nil)
	if adapter.ID() != ID {
		t.Fatalf("ID = %q", adapter.ID())
	}
	if !adapter.Enabled() {
		t.Fatal("configured adapter is disabled")
	}
	if adapter.ConfigurationStatus() != application.ConfigurationConfigured {
		t.Fatalf("configuration = %q", adapter.ConfigurationStatus())
	}
	if !adapter.Healthy(context.Background()) {
		t.Fatal("healthy Jellyfin reported unhealthy")
	}
	version, err := adapter.Version(context.Background())
	if err != nil || version != "10.10.7" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
}

func TestUnconfiguredAdapterIsDisabled(t *testing.T) {
	adapter := New("", nil)
	if adapter.Enabled() {
		t.Fatal("unconfigured adapter is enabled")
	}
	if adapter.ConfigurationStatus() != application.ConfigurationNotConfigured {
		t.Fatalf("configuration = %q", adapter.ConfigurationStatus())
	}
	if adapter.Healthy(context.Background()) {
		t.Fatal("unconfigured adapter is healthy")
	}
	if _, err := adapter.Version(context.Background()); !errors.Is(err, application.ErrDisabled) {
		t.Fatalf("version error = %v", err)
	}
}

func TestLifecycleOperationsUseGenericController(t *testing.T) {
	lifecycle := &lifecycleRecorder{}
	adapter := New("http://jellyfin.test", lifecycle)
	for _, test := range []struct {
		name string
		call func(context.Context) error
	}{
		{"start", adapter.Start},
		{"stop", adapter.Stop},
		{"restart", adapter.Restart},
	} {
		if err := test.call(context.Background()); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if lifecycle.operation != test.name || lifecycle.applicationID != ID {
			t.Fatalf("%s delegated as %q for %q", test.name, lifecycle.operation, lifecycle.applicationID)
		}
	}
}

func TestLifecycleUnavailableIsExplicit(t *testing.T) {
	adapter := New("http://jellyfin.test", nil)
	if err := adapter.Restart(context.Background()); !errors.Is(err, application.ErrLifecycleUnavailable) {
		t.Fatalf("restart error = %v", err)
	}
}

func TestRefreshUsesModernAuthorization(t *testing.T) {
	for _, test := range []struct {
		name          string
		key           string
		authorization string
	}{
		{"plain", "synthetic-key", `MediaBrowser Token="synthetic-key"`},
		{"quoted", "synthetic-\"key\\value", `MediaBrowser Token="synthetic-\"key\\value"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" {
					t.Error("unexpected refresh method or path")
				}
				if r.URL.RawQuery != "" {
					t.Error("refresh must not use query authentication")
				}
				if r.Header.Get("X-Emby-Token") != "" {
					t.Error("refresh must not use legacy token header")
				}
				if r.Header.Get("Authorization") != test.authorization {
					t.Error("refresh must use safely quoted MediaBrowser Authorization")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			if err := New(server.URL, nil).Refresh(context.Background(), test.key); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRefreshHTTPStatus(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusOK, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			err := New(server.URL, nil).Refresh(context.Background(), "synthetic-key")
			if status == http.StatusNoContent {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != fmt.Sprintf("Jellyfin refresh returned HTTP %d", status) {
				t.Fatalf("unexpected refresh status error: %v", err)
			}
		})
	}
}

func TestRefreshUsesDedicatedAPIKey(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Refresh" || r.Header.Get("Authorization") != `MediaBrowser Token="dedicated-key"` || r.URL.RawQuery != "" {
			t.Error("unexpected refresh request method, path, authorization, or query")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	adapter := New(server.URL, nil)
	if err := adapter.Refresh(context.Background(), "dedicated-key"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Refresh(context.Background(), ""); err == nil {
		t.Fatal("accepted empty key")
	}
	if hits != 1 {
		t.Fatalf("hits=%d", hits)
	}
}
