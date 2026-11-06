package main

import (
	"arknet/core/internal/nodes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type enrollFunc func(context.Context, string, string) (nodes.Node, error)

func (f enrollFunc) Enroll(ctx context.Context, id, version string) (nodes.Node, error) {
	return f(ctx, id, version)
}

func (f enrollFunc) RegisterEnrollment(ctx context.Context, id, version string, _ []byte) (nodes.Node, error) {
	return f(ctx, id, version)
}
func (f enrollFunc) EnrollmentIdentity(context.Context, string) (nodes.Node, []byte, error) {
	return nodes.Node{}, nil, nodes.ErrNotFound
}
func signedEnrollmentTestRequest(t *testing.T) *http.Request {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(enrollmentBody(), "}") + `,"public_key":"` + base64.StdEncoding.EncodeToString(pub) + `"}`
	return signedRequest("POST", enrollmentPath, body, enrollmentID, priv, fmt.Sprint(time.Now().Unix()))
}

const enrollmentPath = "/api/federation/v1/enrollments"

var enrollmentID = "node_" + strings.Repeat("a", 64)

func enrollmentBody() string {
	return `{"node_id":"` + enrollmentID + `","version":"0.0.1-dev","capabilities":{}}`
}
func TestEnrollmentRejectsMalformedRequestsBeforeRepository(t *testing.T) {
	valid := enrollmentBody()
	tests := []struct {
		name, body, media string
		status            int
	}{
		{"missing id", `{"version":"v","capabilities":{}}`, "application/json", 400},
		{"missing version", `{"node_id":"` + enrollmentID + `","capabilities":{}}`, "application/json", 400},
		{"missing capabilities", `{"node_id":"` + enrollmentID + `","version":"v"}`, "application/json", 400},
		{"null capabilities", strings.Replace(valid, `{}}`, `null}`, 1), "application/json", 400},
		{"nonempty capabilities", strings.Replace(valid, `{}}`, `{"cpu":4}}`, 1), "application/json", 400},
		{"array capabilities", strings.Replace(valid, `{}}`, `[]}`, 1), "application/json", 400},
		{"injected pii", strings.TrimSuffix(valid, "}") + `,"email":"private@example.com"}`, "application/json", 400},
		{"case alias", strings.Replace(valid, "node_id", "NODE_ID", 1), "application/json", 400},
		{"bad id", strings.Replace(valid, enrollmentID, "operator@example.com", 1), "application/json", 400},
		{"uppercase id", strings.Replace(valid, enrollmentID, strings.ToUpper(enrollmentID), 1), "application/json", 400},
		{"empty version", strings.Replace(valid, "0.0.1-dev", "", 1), "application/json", 400},
		{"blank version", strings.Replace(valid, "0.0.1-dev", "   ", 1), "application/json", 400},
		{"long version", strings.Replace(valid, "0.0.1-dev", strings.Repeat("v", 65), 1), "application/json", 400},
		{"unicode version", strings.Replace(valid, "0.0.1-dev", "é", 1), "application/json", 400},
		{"control version", strings.Replace(valid, "0.0.1-dev", `v\n`, 1), "application/json", 400},
		{"null body", "null", "application/json", 400},
		{"empty body", "", "application/json", 400},
		{"trailing object", valid + ` {}`, "application/json", 400},
		{"trailing garbage", valid + ` x`, "application/json", 400},
		{"oversized", valid + strings.Repeat(" ", 4096), "application/json", 413},
		{"wrong media", valid, "text/plain", 415},
		{"no media", valid, "", 415},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := enrollFunc(func(context.Context, string, string) (nodes.Node, error) {
				t.Fatal("invalid request reached repository")
				return nodes.Node{}, nil
			})
			r := httptest.NewRequest("POST", enrollmentPath, strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.media)
			w := httptest.NewRecorder()
			newHandler(nil, repo).ServeHTTP(w, r)
			if w.Code != tt.status {
				t.Fatalf("status = %d, want %d", w.Code, tt.status)
			}
			if strings.Contains(w.Body.String(), "private@example.com") {
				t.Fatal("request data leaked")
			}
		})
	}
}

func TestEnrollmentConflictReturnsGenericReceipt(t *testing.T) {
	repo := enrollFunc(func(context.Context, string, string) (nodes.Node, error) {
		return nodes.Node{}, fmt.Errorf("internal secret: %w", nodes.ErrEnrollmentConflict)
	})
	r := signedEnrollmentTestRequest(t)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newHandler(nil, repo).ServeHTTP(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("unsafe conflict response: %d %s", w.Code, w.Body)
	}
}

func TestEnrollmentUnavailableIsSanitized(t *testing.T) {
	for _, repo := range []nodes.EnrollmentRepository{nil, enrollFunc(func(context.Context, string, string) (nodes.Node, error) {
		return nodes.Node{}, errors.New("secret database password")
	})} {
		r := signedEnrollmentTestRequest(t)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		newHandler(nil, repo).ServeHTTP(w, r)
		if w.Code != 503 || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("unsafe unavailable response: %d %s", w.Code, w.Body)
		}
	}
}

func TestEnrollmentRequestContextIsBounded(t *testing.T) {
	repo := enrollFunc(func(ctx context.Context, stringID, v string) (nodes.Node, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("missing bounded enrollment deadline")
		}
		if ctx.Err() != context.Canceled {
			t.Fatal("request cancellation not propagated")
		}
		return nodes.Node{}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := signedEnrollmentTestRequest(t).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	newHandler(nil, repo).ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestEnrollmentReturnsPendingOnInitialAndRepeat(t *testing.T) {
	repo := enrollFunc(func(ctx context.Context, id, v string) (nodes.Node, error) {
		if id != enrollmentID || v != "0.0.1-dev" {
			t.Fatalf("unexpected repository arguments")
		}
		return nodes.Node{PublicID: id, EnrollmentStatus: nodes.EnrollmentPending, TrustStatus: nodes.TrustUntrusted}, nil
	})
	handler := newHandler(nil, repo)
	for range 2 {
		r := signedEnrollmentTestRequest(t)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("status = %d, body = %s", w.Code, w.Body)
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got["node_id"] != enrollmentID || got["enrollment_status"] != "PENDING" {
			t.Fatalf("response = %v", got)
		}
		if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("unsafe headers")
		}
	}
}
