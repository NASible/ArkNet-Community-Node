package main

import (
	"fmt"
	"strings"

	"arknet/node-agent/internal/globalauth"
)

// loadGlobalLoginConfig enables Core-bound global login when the member UI's
// callback URLs are configured. ARKNET_CORE_LOGIN_URL is the Core base URL
// browsers use (default ARKNET_CORE_URL); it may differ when the Agent reaches
// Core over a private network. Plain HTTP is accepted only for loopback.
func loadGlobalLoginConfig(getenv func(string) string, coreURL string) (*globalLoginConfig, error) {
	rawCallbacks := getenv("ARKNET_GLOBAL_LOGIN_CALLBACK_URLS")
	loginURL := getenv("ARKNET_CORE_LOGIN_URL")
	if rawCallbacks == "" {
		if loginURL != "" {
			return nil, fmt.Errorf("ARKNET_CORE_LOGIN_URL requires ARKNET_GLOBAL_LOGIN_CALLBACK_URLS")
		}
		return nil, nil
	}
	if coreURL == "" {
		return nil, fmt.Errorf("ARKNET_GLOBAL_LOGIN_CALLBACK_URLS requires ARKNET_CORE_URL")
	}
	if loginURL == "" {
		loginURL = coreURL
	} else if !validCoreBaseURL(loginURL, false) {
		return nil, fmt.Errorf("ARKNET_CORE_LOGIN_URL must be an HTTPS base URL (HTTP only for loopback)")
	}
	var callbacks []string
	seen := map[string]bool{}
	for _, part := range strings.Split(rawCallbacks, ",") {
		callback := strings.TrimSpace(part)
		if !globalauth.ValidCallbackURL(callback) || seen[callback] {
			return nil, fmt.Errorf("ARKNET_GLOBAL_LOGIN_CALLBACK_URLS must list distinct HTTPS callback URLs with a path (HTTP only for loopback)")
		}
		seen[callback] = true
		callbacks = append(callbacks, callback)
	}
	if len(callbacks) > globalauth.MaxCallbackURLs {
		return nil, fmt.Errorf("ARKNET_GLOBAL_LOGIN_CALLBACK_URLS lists more than %d URLs", globalauth.MaxCallbackURLs)
	}
	return &globalLoginConfig{coreURL: strings.TrimSuffix(coreURL, "/"), coreLoginURL: strings.TrimSuffix(loginURL, "/"), callbackURLs: callbacks}, nil
}
