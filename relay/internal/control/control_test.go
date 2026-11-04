package control

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"arknet.local/relay/internal/revocation"
)

var now = time.Unix(1_800_000_000, 0)

type fakeRevoker struct{ calls [][][16]byte }

func (f *fakeRevoker) Revoke(subjects [][16]byte) int {
	f.calls = append(f.calls, subjects)
	return len(subjects)
}

func setup(t *testing.T) (http.Handler, *fakeRevoker, ed25519.PrivateKey, *bytes.Buffer) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	revoker, logs := &fakeRevoker{}, &bytes.Buffer{}
	h := NewHandler(revoker, public, "relay-1", func() time.Time { return now }, slog.New(slog.NewJSONHandler(logs, nil)))
	return h, revoker, private, logs
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, RevocationsPath, strings.NewReader(body)))
	return w
}

func token(t *testing.T, key ed25519.PrivateKey, relayID string, id byte, issued time.Time, subjects ...[16]byte) string {
	t.Helper()
	tok, err := revocation.Sign(key, relayID, [16]byte{id}, issued, subjects)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestSignedRevocationIsApplied(t *testing.T) {
	h, revoker, key, _ := setup(t)
	w := post(h, token(t, key, "relay-1", 1, now, [16]byte{0xaa}, [16]byte{0xbb}))
	if w.Code != http.StatusNoContent || len(revoker.calls) != 1 || len(revoker.calls[0]) != 2 || revoker.calls[0][1] != [16]byte{0xbb} {
		t.Fatalf("%d %v", w.Code, revoker.calls)
	}
}

func TestBadRevocationsAreRejectedWithoutEffect(t *testing.T) {
	h, revoker, key, _ := setup(t)
	_, other, _ := ed25519.GenerateKey(nil)
	good := token(t, key, "relay-1", 9, now, [16]byte{1})
	payload, _, _ := strings.Cut(good, ".")
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"unsigned":       {payload + ".", http.StatusUnauthorized},
		"other key":      {token(t, other, "relay-1", 2, now, [16]byte{1}), http.StatusUnauthorized},
		"other relay":    {token(t, key, "relay-2", 3, now, [16]byte{1}), http.StatusUnauthorized},
		"stale":          {token(t, key, "relay-1", 4, now.Add(-2*time.Minute), [16]byte{1}), http.StatusUnauthorized},
		"garbage":        {"not a token", http.StatusUnauthorized},
		"oversized body": {strings.Repeat("A", revocation.MaxTokenBytes+1), http.StatusRequestEntityTooLarge},
		"trailing junk":  {good + "\nextra", http.StatusUnauthorized},
	} {
		if w := post(h, tc.body); w.Code != tc.want {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if len(revoker.calls) != 0 {
		t.Fatalf("rejected revocations were applied: %v", revoker.calls)
	}
}

func TestReplayedRevocationIsRejected(t *testing.T) {
	h, revoker, key, _ := setup(t)
	tok := token(t, key, "relay-1", 5, now, [16]byte{1})
	if w := post(h, tok); w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
	if w := post(h, tok); w.Code != http.StatusConflict {
		t.Fatalf("replay: %d", w.Code)
	}
	if len(revoker.calls) != 1 {
		t.Fatalf("applied %d times", len(revoker.calls))
	}
}

func TestOnlyHealthAndRevocationsAreExposed(t *testing.T) {
	h, _, _, _ := setup(t)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodGet, RevocationsPath, http.StatusMethodNotAllowed},
		{http.MethodGet, "/stats", http.StatusNotFound},
		{http.MethodPost, "/api/relay/v1/registrations", http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Errorf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s %s: missing no-store", tc.method, tc.path)
		}
	}
}

func TestLogsCarryNoSubjectsOrTokens(t *testing.T) {
	h, _, key, logs := setup(t)
	subject := [16]byte{0xde, 0xad, 0xbe, 0xef, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	tok := token(t, key, "relay-1", 6, now, subject)
	post(h, tok)
	post(h, tok)
	post(h, "garbage")
	if !strings.Contains(logs.String(), "relay_revocation") {
		t.Fatalf("nothing logged: %q", logs.String())
	}
	for _, value := range []string{hex.EncodeToString(subject[:]), tok[:24], "relay-1"} {
		if strings.Contains(logs.String(), value) {
			t.Errorf("log contains %q", value)
		}
	}
}
