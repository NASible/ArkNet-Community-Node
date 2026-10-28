// Package jellyfin implements the ArkNet application adapter for Jellyfin.
package jellyfin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"arknet/node-agent/internal/application"
)

const ID = "jellyfin"

// Adapter exposes Jellyfin through the generic application contract.
type Adapter struct {
	baseURL   string
	http      *http.Client
	lifecycle application.Lifecycle
}

var _ application.Adapter = (*Adapter)(nil)

func New(baseURL string, lifecycle application.Lifecycle) *Adapter {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
		DisableCompression:    true,
	}
	return &Adapter{
		baseURL:   strings.TrimRight(baseURL, "/"),
		http:      &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		lifecycle: lifecycle,
	}
}

// Refresh asks Jellyfin to rescan its read-only ArkNet media mount.
func (a *Adapter) Refresh(ctx context.Context, apiKey string) error {
	if !a.Enabled() || apiKey == "" || len(apiKey) > 512 || strings.ContainsAny(apiKey, "\r\n\x00") {
		return errors.New("Jellyfin refresh is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/Library/Refresh", nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", fmt.Sprintf("MediaBrowser Token=%q", apiKey))
	response, err := a.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("Jellyfin refresh returned HTTP %d", response.StatusCode)
	}
	return nil
}

func (a *Adapter) ID() string { return ID }

func (a *Adapter) Enabled() bool { return a != nil && a.baseURL != "" }

func (a *Adapter) ConfigurationStatus() application.ConfigurationStatus {
	if a.Enabled() {
		return application.ConfigurationConfigured
	}
	return application.ConfigurationNotConfigured
}

func (a *Adapter) Healthy(ctx context.Context) bool {
	if !a.Enabled() {
		return false
	}
	response, err := a.get(ctx, "/health")
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode == http.StatusOK
}

func (a *Adapter) Version(ctx context.Context) (string, error) {
	if !a.Enabled() {
		return "", application.ErrDisabled
	}
	response, err := a.get(ctx, "/System/Info/Public")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Jellyfin version endpoint returned HTTP %d", response.StatusCode)
	}
	var info struct {
		Version string `json:"Version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode Jellyfin version: %w", err)
	}
	if info.Version == "" {
		return "", errors.New("Jellyfin version is missing")
	}
	return info.Version, nil
}

func (a *Adapter) Start(ctx context.Context) error   { return a.lifecycleCall(ctx, "start") }
func (a *Adapter) Stop(ctx context.Context) error    { return a.lifecycleCall(ctx, "stop") }
func (a *Adapter) Restart(ctx context.Context) error { return a.lifecycleCall(ctx, "restart") }

func (a *Adapter) lifecycleCall(ctx context.Context, operation string) error {
	if !a.Enabled() {
		return application.ErrDisabled
	}
	if a.lifecycle == nil {
		return application.ErrLifecycleUnavailable
	}
	switch operation {
	case "start":
		return a.lifecycle.Start(ctx, ID)
	case "stop":
		return a.lifecycle.Stop(ctx, ID)
	default:
		return a.lifecycle.Restart(ctx, ID)
	}
}

func (a *Adapter) get(ctx context.Context, path string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	return a.http.Do(request)
}
