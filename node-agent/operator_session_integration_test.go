package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"
)

// startupOperatorCookie uses the real CLI PostgreSQL store and HTTP enrollment
// flow. Secrets stay in memory, including on failure. Tests use plain loopback
// HTTP, so attach the Secure cookie explicitly rather than weakening its flags.
func startupOperatorCookie(t *testing.T, cfg config, baseURL string) *http.Cookie {
	t.Helper()
	var out bytes.Buffer
	env := defaultCLIEnv(func(key string) string {
		if key == "DATABASE_URL" {
			return cfg.databaseURL
		}
		return ""
	}, &out, io.Discard)
	if exit := runCLI(context.Background(), []string{"operator", "setup-code"}, env); exit != 0 {
		t.Fatalf("operator setup-code exited %d", exit)
	}
	code := displayCode.FindString(out.String())
	if code == "" {
		t.Fatal("operator setup-code did not return a code")
	}
	password := make([]byte, 32)
	if _, err := rand.Read(password); err != nil {
		t.Fatal("generate operator password failed")
	}
	client := &http.Client{Timeout: time.Second}
	post := func(path string, body any, result any) *http.Response {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal("encode operator request failed")
		}
		response, err := client.Post(baseURL+path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal("operator request failed")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("%s: HTTP %d", path, response.StatusCode)
		}
		if result != nil && json.NewDecoder(response.Body).Decode(result) != nil {
			t.Fatal("decode operator response failed")
		}
		return response
	}
	var enrollment struct {
		Token  string `json:"enrollment_token"`
		Secret string `json:"totp_secret"`
	}
	post("/api/v1/operator/setup", map[string]string{
		"setup_code": code, "username": "startup-operator", "password": hex.EncodeToString(password),
	}, &enrollment)
	if enrollment.Token == "" || enrollment.Secret == "" {
		t.Fatal("operator enrollment missing credentials")
	}
	response := post("/api/v1/operator/setup/confirm", map[string]string{
		"enrollment_token": enrollment.Token, "totp": totpNow(t, enrollment.Secret, time.Now()),
	}, nil)
	for _, cookie := range response.Cookies() {
		if cookie.Name == operatorCookie && cookie.Value != "" {
			return cookie
		}
	}
	t.Fatal("operator confirmation missing session cookie")
	return nil
}

func startupStatus(t *testing.T, baseURL string, cookie *http.Cookie) nodeStatus {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/status", nil)
	if err != nil {
		t.Fatal("create status request failed")
	}
	request.AddCookie(cookie)
	client := &http.Client{Timeout: time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("status request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status: HTTP %d", response.StatusCode)
	}
	var status nodeStatus
	if json.NewDecoder(response.Body).Decode(&status) != nil {
		t.Fatal("invalid status")
	}
	return status
}
