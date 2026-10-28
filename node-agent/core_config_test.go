package main

import (
	"strings"
	"testing"
)

func TestCoreURLConfig(t *testing.T) {
	for _, tc := range []struct {
		url, allow string
		valid      bool
	}{
		{"", "", true}, {"https://core.example", "", true}, {"https://core.example/", "", true},
		{"http://localhost:8080", "", true}, {"http://127.0.0.2:8080", "", true}, {"http://[::1]:8080", "", true},
		{"http://core:8080", "true", true}, {"http://core:8080", "", false},
		{"https://user:SECRET@core.example", "", false}, {"https://core.example/?SECRET", "", false},
		{"https://core.example/#SECRET", "", false}, {"https://core.example/api", "", false},
		{"https://core.example?", "", false}, {"https://core.example#", "", false},
		{"https://", "", false}, {"ftp://core.example", "true", false}, {"https://core.example:bad", "", false},
		{"https://core.example:0", "", false}, {"https://core.example:65536", "", false},
		{"https://core.example:", "", false}, {"http://LOCALHOST:8080", "", true},
		{"http://localhost.example", "", false}, {"http://core", "yes", false},
	} {
		t.Run(tc.url+tc.allow, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "ARKNET_CORE_URL" {
					return tc.url
				}
				if key == "ARKNET_CORE_ALLOW_HTTP" {
					return tc.allow
				}
				return ""
			})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "SECRET") {
				t.Fatal("URL leaked")
			}
			if err == nil && cfg.coreURL != tc.url {
				t.Fatal("core URL not retained")
			}
		})
	}
}
