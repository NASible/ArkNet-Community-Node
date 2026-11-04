// Package relays fetches and verifies Core's signed relay list (Prompt 48B).
// The Node connector (Prompt 48D) dials only relays from a verified,
// unexpired list, pinning each relay's certificate key.
package relays

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"arknet/node-agent/internal/enrollment"
)

const (
	Domain        = "ARKNET-RELAY-LIST-V1\x00"
	maxLifetime   = 7 * 24 * time.Hour
	clockSkew     = 5 * time.Minute
	maxRelays     = 16
	maxTokenBytes = 16 << 10
)

var (
	ErrInvalid  = errors.New("invalid relay list")
	ErrFetch    = errors.New("relay list unavailable")
	idPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	pinPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
)

type Relay struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	SPKISHA256 string `json:"spki_sha256"`
}

type List struct {
	IssuedAt, ExpiresAt time.Time
	Relays              []Relay
}

// Verify checks Core's signature (with the pinned relay-list key), the
// version, the validity window and every entry.
func Verify(token string, key ed25519.PublicKey, now time.Time) (List, error) {
	if len(token) > maxTokenBytes || len(key) != ed25519.PublicKeySize {
		return List{}, ErrInvalid
	}
	encodedPayload, encodedSignature, ok := strings.Cut(token, ".")
	payload, err1 := base64.RawURLEncoding.Strict().DecodeString(encodedPayload)
	signature, err2 := base64.RawURLEncoding.Strict().DecodeString(encodedSignature)
	if !ok || err1 != nil || err2 != nil || len(signature) != ed25519.SignatureSize ||
		!ed25519.Verify(key, append([]byte(Domain), payload...), signature) {
		return List{}, ErrInvalid
	}
	var body struct {
		Version   int     `json:"version"`
		IssuedAt  int64   `json:"issued_at"`
		ExpiresAt int64   `json:"expires_at"`
		Relays    []Relay `json:"relays"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || body.Version != 1 || len(body.Relays) > maxRelays {
		return List{}, ErrInvalid
	}
	list := List{IssuedAt: time.Unix(body.IssuedAt, 0), ExpiresAt: time.Unix(body.ExpiresAt, 0), Relays: body.Relays}
	if list.IssuedAt.After(now.Add(clockSkew)) || !now.Before(list.ExpiresAt) || list.ExpiresAt.Sub(list.IssuedAt) > maxLifetime ||
		!list.ExpiresAt.After(list.IssuedAt) {
		return List{}, ErrInvalid
	}
	for _, relay := range list.Relays {
		if !idPattern.MatchString(relay.ID) || !pinPattern.MatchString(relay.SPKISHA256) || !validAddress(relay.Address) {
			return List{}, ErrInvalid
		}
	}
	return list, nil
}

func validAddress(address string) bool {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port >= 1 && port <= 65535 && strconv.Itoa(port) == portText &&
		(net.ParseIP(host) != nil || hostPattern.MatchString(host))
}

// Fetch asks Core for the relay list with a signed federation request and
// verifies it.
func Fetch(ctx context.Context, identity enrollment.Identity, key ed25519.PublicKey) (List, error) {
	if identity.Status != "APPROVED" || len(key) != ed25519.PublicKeySize {
		return List{}, ErrFetch
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	url := strings.TrimSuffix(identity.CoreURL, "/") + "/api/federation/v1/nodes/" + identity.NodeID + "/relays"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil || request.URL.User != nil || request.URL.RawQuery != "" || request.URL.Fragment != "" ||
		enrollment.SignRequest(request, identity.NodeID, identity.PrivateSeed, nil) != nil {
		return List{}, ErrFetch
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Second
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return List{}, ErrFetch
	}
	defer response.Body.Close()
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode != http.StatusOK || err != nil || contentType != "application/json" {
		return List{}, ErrFetch
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTokenBytes+64))
	if err != nil {
		return List{}, ErrFetch
	}
	var body struct {
		RelayList string `json:"relay_list"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil {
		return List{}, ErrInvalid
	}
	return Verify(body.RelayList, key, time.Now())
}

// Holder keeps the newest verified list; an older (replayed) list never
// replaces a newer one, and an expired list is never returned.
type Holder struct {
	mu   sync.RWMutex
	list List
	set  bool
}

func (h *Holder) Store(list List) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.set || list.IssuedAt.After(h.list.IssuedAt) {
		h.list, h.set = list, true
	}
}

func (h *Holder) Current(now time.Time) (List, bool) {
	if h == nil {
		return List{}, false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if !h.set || !now.Before(h.list.ExpiresAt) {
		return List{}, false
	}
	return h.list, true
}
