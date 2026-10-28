package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDevelopmentAdminAuthorization(t *testing.T) {
	token := strings.Repeat("a", 64)
	auth := developmentAdminAuthorizer(token)
	for _, test := range []struct {
		name    string
		headers []string
		ok      bool
	}{
		{"missing", nil, false}, {"valid", []string{"Bearer " + token}, true},
		{"wrong", []string{"Bearer " + strings.Repeat("b", 64)}, false},
		{"empty", []string{"Bearer "}, false}, {"basic", []string{"Basic " + token}, false},
		{"extra space", []string{"Bearer  " + token}, false}, {"trailing", []string{"Bearer " + token + " "}, false},
		{"duplicate", []string{"Bearer " + token, "Bearer " + token}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/admin/v1/nodes/pending", nil)
			for _, v := range test.headers {
				r.Header.Add("Authorization", v)
			}
			if auth.Authorized(r) != test.ok {
				t.Fatal("authorization mismatch")
			}
		})
	}
	if developmentAdminAuthorizer("").Authorized(httptest.NewRequest("GET", "/", nil)) {
		t.Fatal("empty token authorized")
	}
}

func TestDevelopmentAdminConfig(t *testing.T) {
	secret := strings.Repeat("a", 64)
	for _, test := range []struct {
		name, enabled, token string
		bad                  bool
	}{
		{"disabled", "", "", false}, {"enabled", "true", secret, false},
		{"missing token", "true", "", true}, {"token without flag", "", secret, true},
		{"disabled with token", "false", secret, true}, {"bad flag", "secret-flag", "", true},
		{"short", "true", "secret-too-short", true}, {"uppercase", "true", strings.Repeat("A", 64), true},
		{"whitespace", "true", secret + " ", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := loadConfig(func(k string) string {
				if k == "ARKNET_CORE_DEV_ADMIN_ENABLED" {
					return test.enabled
				}
				if k == "ARKNET_CORE_DEV_ADMIN_TOKEN" {
					return test.token
				}
				return ""
			})
			if (err != nil) != test.bad {
				t.Fatalf("unexpected config result %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), test.token) && test.token != "" || strings.Contains(err.Error(), "secret-flag")) {
				t.Fatal("secret echoed")
			}
			if !test.bad && cfg.devAdminEnabled != (test.enabled == "true") {
				t.Fatal("flag mismatch")
			}
		})
	}
}
