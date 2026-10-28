// Package heartbeat implements minimal approved-node connectivity proofs.
package heartbeat

import (
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

var ErrRequestFailed = errors.New("heartbeat_request_failed")

// ErrNotApproved means Core authenticated this Node's signature but refused it
// as no longer approved/trusted. It is a hint only: revocation is confirmed by
// a separate signed enrollment-status request before local state changes.
var ErrNotApproved = errors.New("heartbeat_node_not_approved")

func send(ctx context.Context, id enrollment.Identity, version string, state localstate.State, healthy, peerServing bool) error {
	body, _ := json.Marshal(struct {
		NodeID  string           `json:"node_id"`
		Version string           `json:"version"`
		State   localstate.State `json:"state"`
		Health  struct {
			Healthy         bool `json:"healthy"`
			DatabaseHealthy bool `json:"database_healthy"`
		} `json:"health"`
		Capabilities struct {
			PeerServing bool `json:"peer_serving"`
		} `json:"capabilities"`
	}{id.NodeID, version, state, struct {
		Healthy         bool `json:"healthy"`
		DatabaseHealthy bool `json:"database_healthy"`
	}{healthy, healthy}, struct {
		PeerServing bool `json:"peer_serving"`
	}{peerServing}})
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(id.CoreURL, "/")+"/api/federation/v1/nodes/"+id.NodeID+"/heartbeats", bytes.NewReader(body))
	if err != nil || req.URL.RawQuery != "" || req.URL.ForceQuery || req.URL.User != nil || req.URL.Fragment != "" {
		return ErrRequestFailed
	}
	req.Header.Set("Content-Type", "application/json")
	if enrollment.SignRequest(req, id.NodeID, id.PrivateSeed, body) != nil {
		return ErrRequestFailed
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Second
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return ErrRequestFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return ErrNotApproved
	}
	if resp.StatusCode != 200 {
		return ErrRequestFailed
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil || len(data) > 4096 {
		return ErrRequestFailed
	}
	return parseACK(data, id.NodeID)
}
func parseACK(data []byte, nodeID string) error {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return ErrRequestFailed
	}
	fields := map[string]any{}
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return ErrRequestFailed
		}
		name, ok := k.(string)
		if !ok || (name != "node_id" && name != "accepted") {
			return ErrRequestFailed
		}
		if _, ok := fields[name]; ok {
			return ErrRequestFailed
		}
		v, err := d.Token()
		if err != nil {
			return ErrRequestFailed
		}
		fields[name] = v
	}
	tok, err = d.Token()
	if err != nil || tok != json.Delim('}') || d.Decode(new(any)) != io.EOF || len(fields) != 2 || fields["node_id"] != nodeID || fields["accepted"] != true {
		return ErrRequestFailed
	}
	return nil
}
