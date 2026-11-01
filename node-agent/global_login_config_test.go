package main

import (
	"strings"
	"testing"
)

func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestGlobalLoginConfigIsOptInAndValidated(t *testing.T) {
	cfg, err := loadConfig(envMap(map[string]string{"ARKNET_CORE_URL": "https://core.example"}))
	if err != nil || cfg.globalLogin != nil {
		t.Fatalf("global login must be off without callbacks: %+v %v", cfg.globalLogin, err)
	}
	cfg, err = loadConfig(envMap(map[string]string{"ARKNET_CORE_URL": "https://core.example",
		"ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://node.lan:8443/login/callback, http://127.0.0.1:5174/login/callback"}))
	if err != nil || cfg.globalLogin == nil || cfg.globalLogin.coreLoginURL != "https://core.example" || cfg.globalLogin.coreURL != "https://core.example" ||
		strings.Join(cfg.globalLogin.callbackURLs, " ") != "https://node.lan:8443/login/callback http://127.0.0.1:5174/login/callback" {
		t.Fatalf("global login config: %+v %v", cfg.globalLogin, err)
	}
	cfg, err = loadConfig(envMap(map[string]string{"ARKNET_CORE_URL": "http://arknet-core:8080", "ARKNET_CORE_ALLOW_HTTP": "true",
		"ARKNET_CORE_LOGIN_URL": "http://127.0.0.1:8083", "ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "http://127.0.0.1:8086/login/callback"}))
	if err != nil || cfg.globalLogin.coreLoginURL != "http://127.0.0.1:8083" || cfg.globalLogin.coreURL != "http://arknet-core:8080" {
		t.Fatalf("separate browser-facing Core URL: %+v %v", cfg.globalLogin, err)
	}
	for name, env := range map[string]map[string]string{
		"callbacks without Core": {"ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://node.lan/login/callback"},
		"login URL without callbacks": {"ARKNET_CORE_URL": "https://core.example",
			"ARKNET_CORE_LOGIN_URL": "https://login.example"},
		"plain HTTP LAN callback": {"ARKNET_CORE_URL": "https://core.example", "ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "http://node.lan/login/callback"},
		"callback with query":     {"ARKNET_CORE_URL": "https://core.example", "ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://node.lan/cb?x=1"},
		"callback wildcard host":  {"ARKNET_CORE_URL": "https://core.example", "ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://*/cb"},
		"duplicate callbacks": {"ARKNET_CORE_URL": "https://core.example",
			"ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://node.lan/cb,https://node.lan/cb"},
		"five callbacks": {"ARKNET_CORE_URL": "https://core.example",
			"ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://a.lan/cb,https://b.lan/cb,https://c.lan/cb,https://d.lan/cb,https://e.lan/cb"},
		"plain HTTP login URL": {"ARKNET_CORE_URL": "https://core.example", "ARKNET_CORE_LOGIN_URL": "http://core.example",
			"ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://node.lan/cb"},
		"login URL with path": {"ARKNET_CORE_URL": "https://core.example", "ARKNET_CORE_LOGIN_URL": "https://core.example/x",
			"ARKNET_GLOBAL_LOGIN_CALLBACK_URLS": "https://node.lan/cb"},
	} {
		if _, err := loadConfig(envMap(env)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
