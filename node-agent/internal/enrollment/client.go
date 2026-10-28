// Package enrollment implements the authenticated federation enrollment handshake.
package enrollment

import (
	"arknet/node-agent/internal/localstate"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type Storage interface {
	Snapshot(context.Context) (localstate.Snapshot, error)
	EnrollmentIdentity(context.Context) (Identity, error)
	RecordEnrollmentPending(context.Context, string) (localstate.Snapshot, error)
	CompleteEnrollment(context.Context, string, string) (localstate.Snapshot, error)
}

var ErrRequestFailed = errors.New("enrollment_request_failed")

func eligible(ctx context.Context, coreURL string, store Storage, checking bool) (Identity, bool, error) {
	snap, err := store.Snapshot(ctx)
	if err != nil {
		return Identity{}, false, ErrRequestFailed
	}
	if snap.State != localstate.Provisioning && snap.State != localstate.EnrollmentPending {
		return Identity{}, false, nil
	}
	id, err := store.EnrollmentIdentity(ctx)
	if err != nil {
		return Identity{}, false, ErrRequestFailed
	}
	if id.CoreURL != "" && id.CoreURL != coreURL {
		return Identity{}, false, ErrRequestFailed
	}
	if id.Status == "APPROVED" || id.Status == "REJECTED" {
		return id, false, nil
	}
	if !validNodeID(id.NodeID) || len(id.PrivateSeed) != 32 || (snap.NodeID != "" && snap.NodeID != id.NodeID) || (id.Status != "NOT_ENROLLED" && id.Status != "PENDING") {
		return Identity{}, false, ErrRequestFailed
	}
	if checking {
		pending := id.Status == "PENDING" && snap.State == localstate.EnrollmentPending && id.CoreURL == coreURL
		// A receipt can be lost after Core pinned our key. Only an authenticated
		// status reply can establish the missing local pending record.
		unacknowledged := id.Status == "NOT_ENROLLED" && snap.State == localstate.Provisioning && id.CoreURL == ""
		if !pending && !unacknowledged {
			return id, false, nil
		}
	}
	return id, true, nil
}

// Submit sends one bounded, signed, idempotent receipt; it never re-enrolls approved nodes.
func Submit(ctx context.Context, coreURL, version string, store Storage) error {
	if ctx.Err() != nil {
		return ErrRequestFailed
	}
	if coreURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, ok, err := eligible(ctx, coreURL, store, false)
	if err != nil || !ok {
		return err
	}
	body, err := json.Marshal(struct {
		NodeID       string         `json:"node_id"`
		Version      string         `json:"version"`
		Capabilities map[string]any `json:"capabilities"`
		PublicKey    string         `json:"public_key"`
	}{id.NodeID, version, map[string]any{}, base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(id.PrivateSeed).Public().(ed25519.PublicKey))})
	if err != nil {
		return ErrRequestFailed
	}
	fields, err := request(ctx, http.MethodPost, strings.TrimSuffix(coreURL, "/")+"/api/federation/v1/enrollments", id, body, false)
	if err != nil || fields["node_id"] != id.NodeID || fields["enrollment_status"] != "PENDING" {
		return ErrRequestFailed
	}
	if _, err := store.RecordEnrollmentPending(ctx, coreURL); err != nil {
		return ErrRequestFailed
	}
	return nil
}

// CheckApproval learns only enrollment status, not heartbeat/connectivity health.
func CheckApproval(ctx context.Context, coreURL string, store Storage) error {
	_, err := checkApproval(ctx, coreURL, store)
	return err
}
func checkApproval(ctx context.Context, coreURL string, store Storage) (bool, error) {
	if ctx.Err() != nil {
		return false, ErrRequestFailed
	}
	if coreURL == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	id, ok, err := eligible(ctx, coreURL, store, true)
	if err != nil || !ok {
		return false, err
	}
	fields, err := request(ctx, http.MethodGet, strings.TrimSuffix(coreURL, "/")+"/api/federation/v1/nodes/"+id.NodeID+"/enrollment", id, nil, true)
	if err != nil || fields["node_id"] != id.NodeID {
		return true, ErrRequestFailed
	}
	pair := fields["enrollment_status"] + "/" + fields["trust_status"]
	if id.Status == "NOT_ENROLLED" && (pair == "PENDING/UNTRUSTED" || pair == "APPROVED/TRUSTED") {
		if _, err := store.RecordEnrollmentPending(ctx, coreURL); err != nil {
			return true, ErrRequestFailed
		}
	}
	switch pair {
	case "PENDING/UNTRUSTED":
		return true, nil
	case "APPROVED/TRUSTED":
		if _, err := store.CompleteEnrollment(ctx, coreURL, id.NodeID); err != nil {
			return true, ErrRequestFailed
		}
		return false, nil
	case "REJECTED/BLOCKED", "REVOKED/BLOCKED":
		return false, nil
	default:
		return true, ErrRequestFailed
	}
}

func request(ctx context.Context, method, url string, id Identity, body []byte, approval bool) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, ErrRequestFailed
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if SignRequest(req, id.NodeID, id.PrivateSeed, body) != nil {
		return nil, ErrRequestFailed
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrRequestFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ErrRequestFailed
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return nil, ErrRequestFailed
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrRequestFailed
	}
	fields := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || (name != "node_id" && name != "enrollment_status" && !(approval && name == "trust_status")) {
			return nil, ErrRequestFailed
		}
		if _, exists := fields[name]; exists {
			return nil, ErrRequestFailed
		}
		value, err := decoder.Token()
		text, ok := value.(string)
		if err != nil || !ok {
			return nil, ErrRequestFailed
		}
		fields[name] = text
	}
	token, err = decoder.Token()
	count := 2
	if approval {
		count = 3
	}
	if err != nil || token != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(fields) != count || !validNodeID(fields["node_id"]) {
		return nil, ErrRequestFailed
	}
	return fields, nil
}

// FederationStatus asks Core, with a signed request, for this Node's current
// enrollment/trust decision, returned as "ENROLLMENT/TRUST" (for example
// "REVOKED/BLOCKED"). It never changes local state.
func FederationStatus(ctx context.Context, id Identity) (string, error) {
	if ctx.Err() != nil || !validNodeID(id.NodeID) || len(id.PrivateSeed) != 32 || id.CoreURL == "" {
		return "", ErrRequestFailed
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	fields, err := request(ctx, http.MethodGet, strings.TrimSuffix(id.CoreURL, "/")+"/api/federation/v1/nodes/"+id.NodeID+"/enrollment", id, nil, true)
	if err != nil || fields["node_id"] != id.NodeID {
		return "", ErrRequestFailed
	}
	return fields["enrollment_status"] + "/" + fields["trust_status"], nil
}

// Run performs one startup POST, then approval GETs only while local enrollment is pending.
// Cancellation joins cleanly because all storage and network operations share ctx.
func Run(ctx context.Context, coreURL, version string, store Storage, onError func(error)) {
	run(ctx, coreURL, version, store, onError, 5*time.Second)
}
func run(ctx context.Context, coreURL, version string, store Storage, onError func(error), interval time.Duration) {
	if ctx.Err() != nil || coreURL == "" {
		return
	}
	report := func(err error) {
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(ErrRequestFailed)
		}
	}
	report(Submit(ctx, coreURL, version, store))
	for ctx.Err() == nil {
		pending, err := checkApproval(ctx, coreURL, store)
		report(err)
		if !pending {
			return
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
