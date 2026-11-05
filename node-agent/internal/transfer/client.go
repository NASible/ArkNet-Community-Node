// Package transfer is the independent, fake-only transfer service HTTP client.
package transfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

type State string

const (
	Downloading        State = "DOWNLOADING"
	Paused             State = "PAUSED"
	Completed          State = "COMPLETED"
	Cancelled          State = "CANCELLED"
	VerificationFailed State = "VERIFICATION_FAILED"
)

type Relay struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	SPKISHA256 string `json:"spki_sha256"`
}

type Request struct {
	Relays             []Relay `json:"relays,omitempty"`
	RelayListExpiresAt int64   `json:"relay_list_expires_at,omitempty"`
	ObjectID           string  `json:"object_id"`
	Size               int64   `json:"size"`
	InfoHashV2         string  `json:"info_hash_v2"`
	TrackerURL         string  `json:"tracker_url"`
	WebSeedURL         string  `json:"web_seed_url,omitempty"`
	TorrentURL         string  `json:"torrent_url,omitempty"`
}
type Status struct {
	ObjectID       string `json:"object_id"`
	State          State  `json:"state"`
	TotalBytes     int64  `json:"total_bytes"`
	CompletedBytes int64  `json:"completed_bytes"`
	Mock           bool   `json:"mock"`
	ErrorCode      string `json:"error_code"`
}
type Service interface {
	Add(context.Context, Request) (Status, error)
	List(context.Context) ([]Status, error)
	Get(context.Context, string) (Status, error)
	Pause(context.Context, string) (Status, error)
	Resume(context.Context, string) (Status, error)
	Refresh(context.Context, Request) (Status, error)
	Cancel(context.Context, string) (Status, error)
}
type Client struct {
	baseURL, token string
	http           *http.Client
}

func New(baseURL, token string) *Client {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 3 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          20,
		DisableCompression:    true,
	}
	return &Client{baseURL: baseURL, token: token, http: &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

const route = "/api/transfer/v1/transfers"

func (c *Client) call(ctx context.Context, method, path string, input, output any) error {
	if ValidateConfig(c.baseURL, c.token) != nil {
		return ErrUnavailable
	}
	var body []byte
	if input != nil {
		body, _ = json.Marshal(input)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return ErrInvalidResponse
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return ErrUnavailable
	}
	if len(data) > 1<<20 {
		return ErrInvalidResponse
	}
	if resp.StatusCode >= 500 {
		return ErrUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		var result struct {
			Error string `json:"error"`
		}
		if decode(data, &result) != nil {
			return ErrInvalidResponse
		}
		switch {
		case resp.StatusCode == 401 && result.Error == "unauthorized":
			return ErrUnauthorized
		case resp.StatusCode == 404 && result.Error == "not_found":
			return ErrNotFound
		case resp.StatusCode == 409 && result.Error == "conflict":
			return ErrConflict
		case resp.StatusCode == 409 && result.Error == "invalid_transition":
			return ErrInvalidTransition
		case resp.StatusCode == 400 && result.Error == "invalid_request":
			return ErrInvalidRequest
		default:
			return ErrInvalidResponse
		}
	}
	return decode(data, output)
}
func (c *Client) Add(ctx context.Context, input Request) (Status, error) {
	if !validRequest(input) {
		return Status{}, ErrInvalidRequest
	}
	var result Status
	if err := c.call(ctx, "POST", route, input, &result); err != nil {
		return Status{}, err
	}
	if !validStatus(result) || result.ObjectID != input.ObjectID || result.TotalBytes != input.Size {
		return Status{}, ErrInvalidResponse
	}
	return result, nil
}
func (c *Client) Refresh(ctx context.Context, input Request) (Status, error) {
	return c.Add(ctx, input)
}
func decode(data []byte, output any) error {
	if !uniqueJSON(data) || !responseShape(data, output) {
		return ErrInvalidResponse
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(output) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalidResponse
	}
	return nil
}
func (c *Client) List(ctx context.Context) ([]Status, error) {
	var result listResponse
	if err := c.call(ctx, "GET", route, nil, &result); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, s := range result.Transfers {
		if !validStatus(s) || seen[s.ObjectID] {
			return nil, ErrInvalidResponse
		}
		seen[s.ObjectID] = true
	}
	return result.Transfers, nil
}
func (c *Client) resource(ctx context.Context, id, action string) (Status, error) {
	if !validID(id) {
		return Status{}, ErrInvalidRequest
	}
	path := route + "/" + id
	method := "GET"
	var input any
	if action != "" {
		path += "/" + action
		method = "POST"
		input = struct{}{}
	}
	var result Status
	if err := c.call(ctx, method, path, input, &result); err != nil {
		return Status{}, err
	}
	if !validStatus(result) || result.ObjectID != id {
		return Status{}, ErrInvalidResponse
	}
	return result, nil
}
func (c *Client) Get(ctx context.Context, id string) (Status, error) { return c.resource(ctx, id, "") }
func (c *Client) Pause(ctx context.Context, id string) (Status, error) {
	return c.resource(ctx, id, "pause")
}
func (c *Client) Resume(ctx context.Context, id string) (Status, error) {
	return c.resource(ctx, id, "resume")
}
func (c *Client) Cancel(ctx context.Context, id string) (Status, error) {
	return c.resource(ctx, id, "cancel")
}

// SeedRequest asks the local transfer service to serve a finished,
// Agent-finalized object with a fresh Core seed grant. No file name or path is
// sent: the service derives <object_id>.mp4 itself.
type SeedRequest struct {
	Relays             []Relay `json:"relays,omitempty"`
	RelayListExpiresAt int64   `json:"relay_list_expires_at,omitempty"`
	ObjectID           string  `json:"object_id"`
	Size               int64   `json:"size"`
	InfoHashV2         string  `json:"info_hash_v2"`
	TrackerURL         string  `json:"tracker_url"`
	TorrentURL         string  `json:"torrent_url"`
}

type SeedState string

const (
	SeedChecking           SeedState = "CHECKING"
	SeedSeeding            SeedState = "SEEDING"
	SeedVerificationFailed SeedState = "VERIFICATION_FAILED"
	SeedRemoved            SeedState = "REMOVED"
)

// SeedStatus is deliberately only the object ID and serving state.
type SeedStatus struct {
	ObjectID string    `json:"object_id"`
	State    SeedState `json:"state"`
}

// Seeder is the Peer Serving surface of the real transfer service.
type Seeder interface {
	Seed(context.Context, SeedRequest) (SeedStatus, error)
	Unseed(context.Context, string) (SeedStatus, error)
	ListSeeds(context.Context) ([]SeedStatus, error)
}

var _ Seeder = (*Client)(nil)

const seedRoute = "/api/transfer/v1/seeds"

func (c *Client) Seed(ctx context.Context, input SeedRequest) (SeedStatus, error) {
	if !validSeedRequest(input) {
		return SeedStatus{}, ErrInvalidRequest
	}
	var result SeedStatus
	if err := c.call(ctx, "POST", seedRoute, input, &result); err != nil {
		return SeedStatus{}, err
	}
	if !validSeedStatus(result, false) || result.ObjectID != input.ObjectID {
		return SeedStatus{}, ErrInvalidResponse
	}
	return result, nil
}

func (c *Client) Unseed(ctx context.Context, id string) (SeedStatus, error) {
	if !validID(id) {
		return SeedStatus{}, ErrInvalidRequest
	}
	var result SeedStatus
	if err := c.call(ctx, "DELETE", seedRoute+"/"+id, nil, &result); err != nil {
		return SeedStatus{}, err
	}
	if !validSeedStatus(result, true) || result.ObjectID != id || result.State != SeedRemoved {
		return SeedStatus{}, ErrInvalidResponse
	}
	return result, nil
}

func (c *Client) ListSeeds(ctx context.Context) ([]SeedStatus, error) {
	var result seedListResponse
	if err := c.call(ctx, "GET", seedRoute, nil, &result); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	for _, s := range result.Seeds {
		if !validSeedStatus(s, false) || seen[s.ObjectID] {
			return nil, ErrInvalidResponse
		}
		seen[s.ObjectID] = true
	}
	return result.Seeds, nil
}
