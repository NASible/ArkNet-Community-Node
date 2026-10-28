// Package updates discovers newer ArkNet Node releases on the Node's channel.
// It is read-only: it never downloads images or changes the running stack.
//
// A release is reported available only after its manifest has been fetched,
// matched against the SHA-256 published in the channel index, and accepted by
// the same strict parser that validates the running release.
package updates

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"arknet/node-agent/internal/release"
)

const (
	IndexSchema    = "arknet.node-release-index/v1"
	MaxIndexBytes  = 64 << 10
	RequestTimeout = 10 * time.Second
	// CheckInterval is the background discovery period.
	CheckInterval = 6 * time.Hour
	// MinManualInterval coalesces repeated operator "check now" requests.
	MinManualInterval = 10 * time.Second
)

// State is the outcome of the latest discovery attempt.
type State string

const (
	NotConfigured    State = "NOT_CONFIGURED"     // no release index URL
	NoCurrentRelease State = "NO_CURRENT_RELEASE" // running release unknown, so nothing to compare
	NotChecked       State = "NOT_CHECKED"
	UpToDate         State = "UP_TO_DATE"
	UpdateAvailable  State = "UPDATE_AVAILABLE"
	CheckFailed      State = "CHECK_FAILED"
)

var (
	ErrIndex    = errors.New("invalid release index")
	ErrManifest = errors.New("published release manifest rejected")
	ErrFetch    = errors.New("release index unavailable")
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Entry struct {
	Version        string `json:"version"`
	ManifestURL    string `json:"manifest_url"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

type Index struct {
	Schema   string                    `json:"schema"`
	Channels map[release.Channel]Entry `json:"channels"`
}

// Available is the discovered newer release.
type Available struct {
	Version     string          `json:"version"`
	Channel     release.Channel `json:"channel"`
	PublishedAt time.Time       `json:"published_at"`
}

type Status struct {
	State     State
	CheckedAt time.Time
	Available *Available
}

// ParseIndex strictly validates a channel index.
func ParseIndex(data []byte) (Index, error) {
	var index Index
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return Index{}, fmt.Errorf("%w: malformed JSON", ErrIndex)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Index{}, fmt.Errorf("%w: trailing data", ErrIndex)
	}
	if index.Schema != IndexSchema || index.Channels == nil {
		return Index{}, fmt.Errorf("%w: unsupported schema", ErrIndex)
	}
	for channel, entry := range index.Channels {
		if (channel != release.Stable && channel != release.Development) || !release.ValidVersion(entry.Version, channel) ||
			entry.ManifestURL == "" || len(entry.ManifestURL) > 2048 || !sha256Hex.MatchString(entry.ManifestSHA256) {
			return Index{}, fmt.Errorf("%w: invalid channel entry", ErrIndex)
		}
	}
	return index, nil
}

// ValidateURL accepts HTTPS URLs, or HTTP to loopback / with explicit opt-in.
func ValidateURL(u *url.URL, allowHTTP bool) error {
	if u == nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return errors.New("release URL must be an absolute HTTPS URL without credentials")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	loopback := strings.EqualFold(u.Hostname(), "localhost") || (ip != nil && ip.IsLoopback())
	if u.Scheme == "http" && (loopback || allowHTTP) {
		return nil
	}
	return errors.New("release URL must use HTTPS (HTTP requires loopback or explicit development opt-in)")
}

type Checker struct {
	IndexURL  *url.URL
	AllowHTTP bool
	Client    *http.Client
}

func (c Checker) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	// No redirects: a redirect could silently downgrade to plain HTTP.
	return &http.Client{Timeout: RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (c Checker) get(ctx context.Context, u *url.URL) ([]byte, error) {
	if err := ValidateURL(u, c.AllowHTTP); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFetch, err)
	}
	// Deliberately no Node identity, credentials or cookies.
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "arknet-agent")
	response, err := c.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: request failed", ErrFetch)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", ErrFetch, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxIndexBytes+1))
	if err != nil || len(data) > MaxIndexBytes {
		return nil, fmt.Errorf("%w: response too large or truncated", ErrFetch)
	}
	return data, nil
}

// Check compares the published release on the running release's channel with
// the running release. Only strictly newer versions are reported.
func (c Checker) Check(ctx context.Context, current release.Manifest) (Status, error) {
	data, err := c.get(ctx, c.IndexURL)
	if err != nil {
		return Status{State: CheckFailed}, err
	}
	index, err := ParseIndex(data)
	if err != nil {
		return Status{State: CheckFailed}, err
	}
	entry, ok := index.Channels[current.Channel]
	if !ok {
		return Status{State: UpToDate}, nil // nothing published on this channel
	}
	newer, err := release.Compare(entry.Version, current.Version)
	if err != nil {
		return Status{State: CheckFailed}, err
	}
	if newer <= 0 {
		return Status{State: UpToDate}, nil
	}
	manifestURL, err := c.IndexURL.Parse(entry.ManifestURL)
	if err != nil {
		return Status{State: CheckFailed}, fmt.Errorf("%w: manifest URL", ErrIndex)
	}
	data, err = c.get(ctx, manifestURL)
	if err != nil {
		return Status{State: CheckFailed}, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != entry.ManifestSHA256 {
		return Status{State: CheckFailed}, fmt.Errorf("%w: digest mismatch", ErrManifest)
	}
	manifest, err := release.Parse(data)
	if err != nil || manifest.Version != entry.Version || manifest.Channel != current.Channel {
		return Status{State: CheckFailed}, fmt.Errorf("%w: does not match index entry", ErrManifest)
	}
	return Status{State: UpdateAvailable, Available: &Available{Version: manifest.Version, Channel: manifest.Channel, PublishedAt: manifest.PublishedAt}}, nil
}

// Monitor holds the latest discovery result and serialises checks.
type Monitor struct {
	checker *Checker
	current *release.Manifest
	now     func() time.Time
	run     sync.Mutex // one check at a time
	mu      sync.Mutex
	status  Status
}

// NewMonitor returns a monitor; a nil checker or current release yields a
// fixed NOT_CONFIGURED or NO_CURRENT_RELEASE state and never fetches.
func NewMonitor(checker *Checker, current *release.Manifest) *Monitor {
	m := &Monitor{checker: checker, current: current, now: time.Now, status: Status{State: NotChecked}}
	switch {
	case checker == nil:
		m.status.State = NotConfigured
	case current == nil:
		m.status.State = NoCurrentRelease
	}
	return m
}

func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Monitor) Current() *release.Manifest { return m.current }

// Check runs discovery now. Concurrent callers wait for the running check
// instead of starting another; manual checks within MinManualInterval of the
// last completed check return the cached result.
func (m *Monitor) Check(ctx context.Context, manual bool) (Status, error) {
	if m.checker == nil || m.current == nil {
		return m.Status(), nil
	}
	m.run.Lock()
	defer m.run.Unlock()
	if last := m.Status(); manual && !last.CheckedAt.IsZero() && m.now().Sub(last.CheckedAt) < MinManualInterval {
		return last, nil
	}
	status, err := m.checker.Check(ctx, *m.current)
	status.CheckedAt = m.now().UTC()
	m.mu.Lock()
	if err != nil && m.status.State == UpdateAvailable {
		// A transient failure does not hide a release already verified.
		status.Available = m.status.Available
	}
	m.status = status
	m.mu.Unlock()
	return status, err
}

// Run checks at start and then every interval until ctx ends.
func (m *Monitor) Run(ctx context.Context, interval time.Duration, onError func(error)) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if m.checker == nil || m.current == nil {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if _, err := m.Check(ctx, false); err != nil && onError != nil && ctx.Err() == nil {
				onError(err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
