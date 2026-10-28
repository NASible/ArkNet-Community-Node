package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "0123456789abcdef0123456789abcdef"

func request(t *testing.T, h http.Handler, method, path, body, token string, want int) map[string]any {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != want {
		t.Fatalf("%s %s: got %d %s want %d", method, path, w.Code, w.Body.String(), want)
	}
	if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", w.Header())
	}
	var v map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestHealthVersionAuth(t *testing.T) {
	h := newHandler(testToken, nil)
	v := request(t, h, "GET", "/health", "", "", 200)
	if v["healthy"] != true || v["mock"] != false || v["backend"] != "libtorrent" {
		t.Fatal(v)
	}
	for _, token := range []string{"", "wrong", testToken + "x"} {
		if v := request(t, h, "GET", "/api/transfer/v1/version", "", token, 401); v["error"] != "unauthorized" {
			t.Fatal(v)
		}
	}
	v = request(t, h, "GET", "/api/transfer/v1/version", "", testToken, 200)
	policy, ok := v["policy"].(map[string]any)
	if v["version"] != "0.0.1-dev" || v["mock"] != false || v["backend"] != "libtorrent" || !ok ||
		policy["v2_only"] != true || policy["dht"] != false || policy["pex"] != false || policy["lsd"] != false {
		t.Fatal(v)
	}
	request(t, h, "POST", "/api/transfer/v1/version", "", testToken, 405)
	request(t, h, "HEAD", "/health", "", "", 405)
	request(t, h, "GET", "/api/unknown", "", "", 401)
	request(t, h, "GET", "/unknown", "", "", 404)
}
