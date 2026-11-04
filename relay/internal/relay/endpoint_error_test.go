package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"arknet.local/relay/internal/grant"
)

// A synchronous connection lets a signing failure occur immediately after the
// registration response, without racing on mutable live-server configuration.
type endpointWriteConn struct {
	net.Conn
	writes     bytes.Buffer
	afterWrite func()
}

func (c *endpointWriteConn) Write(p []byte) (int, error) {
	n, err := c.writes.Write(p)
	if c.afterWrite != nil {
		f := c.afterWrite
		c.afterWrite = nil
		f()
	}
	return n, err
}
func (c *endpointWriteConn) Close() error                     { return nil }
func (c *endpointWriteConn) SetWriteDeadline(time.Time) error { return nil }

func TestEndpointSigningFailureDoesNotPublishRegistration(t *testing.T) {
	h := newHarness(t, nil)
	cfg := h.relay.cfg
	cfg.EndpointSigningKey = nil
	r := New(cfg)
	conn := &endpointWriteConn{}
	r.serveListen(&trackedConn{Conn: conn, relay: r}, bufio.NewReader(strings.NewReader("")), request{Grant: h.grantFor(7, 9, grant.ActionSeed, time.Minute)})
	var reply map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(conn.writes.Bytes()), &reply); err != nil {
		t.Fatal(err)
	}
	if reply["error"] != "unavailable" || reply["endpoint_token"] != nil || r.Stats().Registrations != 0 {
		t.Fatal("signing failure published endpoint")
	}
}

func TestEndpointSigningFailureOnRenewDropsRegistration(t *testing.T) {
	h := newHarness(t, nil)
	r := New(h.relay.cfg)
	conn := &endpointWriteConn{afterWrite: func() { r.cfg.EndpointSigningKey = nil }}
	grantToken := h.grantFor(7, 9, grant.ActionSeed, time.Minute)
	renew, _ := json.Marshal(request{Op: "renew", Grant: grantToken})
	r.serveListen(&trackedConn{Conn: conn, relay: r}, bufio.NewReader(bytes.NewReader(append(renew, '\n'))), request{Grant: grantToken})
	decoder := json.NewDecoder(&conn.writes)
	var registered, renewed map[string]any
	if err := decoder.Decode(&registered); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&renewed); err != nil {
		t.Fatal(err)
	}
	if registered["endpoint_token"] == nil || renewed["error"] != "unavailable" || renewed["endpoint_token"] != nil || r.Stats().Registrations != 0 {
		t.Fatal("renew signing failure did not fail closed")
	}
}
