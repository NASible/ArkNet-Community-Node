package main

import (
	"arknet/transfer/engine"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRequestEnvelopeBounds(t *testing.T) {
	h := newHandler(testToken, engine.New(time.Hour, nil))
	for _, path := range []string{"/health", "/api/transfer/v1/version", base} {
		request(t, h, "GET", path, strings.Repeat("x", 4097), testToken, 400)
		request(t, h, "GET", path, "{}", testToken, 400)
	}
	r := httptest.NewRequest("GET", "/api/transfer/v1/version", nil)
	r.Header.Add("Authorization", "Bearer "+testToken)
	r.Header.Add("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, value := range []string{"bearer " + testToken, "Bearer  " + testToken, "Bearer " + testToken + " ", testToken} {
		r := httptest.NewRequest("GET", "/api/transfer/v1/version", nil)
		r.Header.Set("Authorization", value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || strings.Contains(w.Body.String(), testToken) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
