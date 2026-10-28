// Package catalog fetches the authorized Core media catalog for this node.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"arknet/node-agent/internal/enrollment"
)

type State string

type MediaType string

const (
	Active  State = "ACTIVE"
	Removed State = "REMOVED"

	Video MediaType = "VIDEO"
	Audio MediaType = "AUDIO"
	Image MediaType = "IMAGE"
	Other MediaType = "OTHER"
)

var ErrRequestFailed = errors.New("catalog_request_failed")

type Object struct {
	ID          string    `json:"id"`
	ContentType MediaType `json:"content_type"`
	Title       string    `json:"title"`
	Size        int64     `json:"size"`
	State       State     `json:"state"`
	InfoHashV2  string    `json:"info_hash_v2"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type TransferGrant struct {
	TrackerURL string    `json:"tracker_url"`
	WebSeedURL string    `json:"web_seed_url,omitempty"`
	TorrentURL string    `json:"torrent_url,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	Actions    []string  `json:"actions"`
}

// ErrGrantRefused means Core authenticated the request and refused the grant
// (401/403): for a seed grant, this Node is not an eligible source. It wraps
// ErrRequestFailed so existing callers keep treating it as a failure.
var ErrGrantRefused = fmt.Errorf("%w: grant refused", ErrRequestFailed)

// Grant obtains short-lived, object-scoped tracker and optional web-seed URLs.
func Grant(ctx context.Context, identity enrollment.Identity, objectID string) (TransferGrant, error) {
	return requestGrant(ctx, identity, objectID, nil, "download")
}

// SeedGrant asks Core for a seed-only grant. Core issues one only while this
// Node is an eligible source (verified replica, Peer Serving on, approved and
// trusted, ACTIVE object). It must carry torrent metadata and never a web seed.
func SeedGrant(ctx context.Context, identity enrollment.Identity, objectID string) (TransferGrant, error) {
	return requestGrant(ctx, identity, objectID, []byte(`{"actions":["seed"]}`), "seed")
}

func requestGrant(ctx context.Context, identity enrollment.Identity, objectID string, body []byte, action string) (TransferGrant, error) {
	if ctx.Err() != nil || identity.Status != "APPROVED" || !validOpaqueID(objectID) {
		return TransferGrant{}, ErrRequestFailed
	}
	endpoint := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + "/media/" + objectID + "/tracker-grants"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" || enrollment.SignRequest(req, identity.NodeID, identity.PrivateSeed, body) != nil {
		return TransferGrant{}, ErrRequestFailed
	}
	req.Header.Set("Content-Type", "application/json")
	client := boundedClient()
	resp, err := client.Do(req)
	if err != nil {
		return TransferGrant{}, ErrRequestFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return TransferGrant{}, ErrGrantRefused
	}
	mediaType, _, typeErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var grant TransferGrant
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	core, _ := url.Parse(identity.CoreURL)
	allowHTTP := core != nil && core.Scheme == "http"
	now := time.Now()
	if resp.StatusCode != http.StatusOK || typeErr != nil || mediaType != "application/json" || readErr != nil || len(data) > 4096 || decoder.Decode(&grant) != nil || decoder.Decode(new(any)) != io.EOF || !validGrantURL(grant.TrackerURL, allowHTTP) || !grant.ExpiresAt.After(now) || grant.ExpiresAt.After(now.Add(15*time.Minute)) || len(grant.Actions) != 1 || grant.Actions[0] != action {
		return TransferGrant{}, ErrRequestFailed
	}
	if action == "seed" {
		// A seed never receives payload: any web_seed_url member, even empty, is an error.
		var members map[string]json.RawMessage
		if json.Unmarshal(data, &members) != nil {
			return TransferGrant{}, ErrRequestFailed
		}
		if _, present := members["web_seed_url"]; present || grant.TorrentURL == "" {
			return TransferGrant{}, ErrRequestFailed
		}
	} else if grant.WebSeedURL == "" || grant.TorrentURL == "" {
		return TransferGrant{}, ErrRequestFailed
	}
	if grant.WebSeedURL != "" {
		u, err := url.Parse(grant.WebSeedURL)
		want := "/api/web-seed/v1/media/" + objectID + "/"
		if err != nil || !validGrantURL(grant.WebSeedURL, allowHTTP) || !strings.HasPrefix(u.EscapedPath(), want) || len(strings.TrimPrefix(u.EscapedPath(), want)) == 0 {
			return TransferGrant{}, ErrRequestFailed
		}
	}
	if grant.TorrentURL != "" {
		u, err := url.Parse(grant.TorrentURL)
		want := "/api/torrent/v1/media/" + objectID + "/"
		if err != nil || !validGrantURL(grant.TorrentURL, allowHTTP) || !strings.HasPrefix(u.EscapedPath(), want) || len(strings.TrimPrefix(u.EscapedPath(), want)) == 0 {
			return TransferGrant{}, ErrRequestFailed
		}
	}
	return grant, nil
}

// ReportReplica idempotently reports a locally finalized, verified replica.
func ReportReplica(ctx context.Context, identity enrollment.Identity, objectID string, size int64, infoHashV2 string) error {
	if ctx.Err() != nil || identity.Status != "APPROVED" || !validOpaqueID(objectID) || size < 0 || len(infoHashV2) != 64 {
		return ErrRequestFailed
	}
	for _, c := range infoHashV2 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ErrRequestFailed
		}
	}
	body, err := json.Marshal(struct {
		Size       int64  `json:"size"`
		InfoHashV2 string `json:"info_hash_v2"`
	}{size, infoHashV2})
	if err != nil {
		return ErrRequestFailed
	}
	endpoint := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + "/media/" + objectID + "/replicas"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil || req.URL.User != nil || req.URL.RawQuery != "" || req.URL.Fragment != "" || enrollment.SignRequest(req, identity.NodeID, identity.PrivateSeed, body) != nil {
		return ErrRequestFailed
	}
	req.Header.Set("Content-Type", "application/json")
	client := boundedClient()
	resp, err := client.Do(req)
	if err != nil {
		return ErrRequestFailed
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if resp.StatusCode != http.StatusNoContent || readErr != nil || len(data) != 0 {
		return ErrRequestFailed
	}
	return nil
}

func validGrantURL(value string, allowHTTP bool) bool {
	u, err := url.Parse(value)
	return err == nil && len(value) <= 2048 && (u.Scheme == "https" || allowHTTP && u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.Opaque == ""
}

func boundedClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Fetch returns the ACTIVE objects and REMOVED tombstones authorized for the
// enrolled node. Federation credentials never appear in the returned model.
func Fetch(ctx context.Context, identity enrollment.Identity) ([]Object, error) {
	if ctx.Err() != nil || identity.Status != "APPROVED" {
		return nil, ErrRequestFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + "/media"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil || request.URL.RawQuery != "" || request.URL.ForceQuery || request.URL.User != nil || request.URL.Fragment != "" {
		return nil, ErrRequestFailed
	}
	if enrollment.SignRequest(request, identity.NodeID, identity.PrivateSeed, nil) != nil {
		return nil, ErrRequestFailed
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Second
	defer transport.CloseIdleConnections()
	client := http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, ErrRequestFailed
	}
	defer response.Body.Close()
	contentType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || contentTypeErr != nil || contentType != "application/json" {
		return nil, ErrRequestFailed
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return nil, ErrRequestFailed
	}
	var result struct {
		Objects []Object `json:"objects"`
		Page    int      `json:"page"`
		Limit   int      `json:"limit"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || result.Page != 1 || result.Limit != 100 || len(result.Objects) > result.Limit {
		return nil, ErrRequestFailed
	}
	for _, object := range result.Objects {
		if !validObject(object) {
			return nil, ErrRequestFailed
		}
	}
	return result.Objects, nil
}

func validObject(object Object) bool {
	if !validOpaqueID(object.ID) || object.Size < 0 || object.CreatedAt.IsZero() || object.UpdatedAt.Before(object.CreatedAt) {
		return false
	}
	switch object.ContentType {
	case Video, Audio, Image, Other:
	default:
		return false
	}
	if object.State != Active && object.State != Removed {
		return false
	}
	if object.InfoHashV2 == "" {
		return true
	}
	if len(object.InfoHashV2) != 64 {
		return false
	}
	for _, character := range object.InfoHashV2 {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func validOpaqueID(id string) bool {
	if len(id) != 70 || !strings.HasPrefix(id, "media_") {
		return false
	}
	for _, character := range id[6:] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
