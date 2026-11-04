package relay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"arknet.local/relay/internal/grant"
)

func assertEndpointToken(t *testing.T, reply map[string]any, public ed25519.PublicKey, endpoint string, subjectByte, hashByte byte, expiry time.Time) string {
	t.Helper()
	token, _ := reply["endpoint_token"].(string)
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		t.Fatal("missing/malformed endpoint token")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payload) != 63 {
		t.Fatal("invalid payload")
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || !ed25519.Verify(public, append([]byte("ARKNET-RELAY-ENDPOINT-V1\x00"), payload...), sig) {
		t.Fatal("invalid endpoint signature")
	}
	host, port, _ := net.SplitHostPort(endpoint)
	n, _ := strconv.Atoi(port)
	if payload[0] != 1 || binary.BigEndian.Uint64(payload[1:9]) != uint64(expiry.Unix()) ||
		!bytes.Equal(payload[9:25], bytes.Repeat([]byte{subjectByte}, 16)) || !bytes.Equal(payload[25:57], bytes.Repeat([]byte{hashByte}, 32)) ||
		!bytes.Equal(payload[57:61], net.ParseIP(host).To4()) || binary.BigEndian.Uint16(payload[61:63]) != uint16(n) || reply["expires_at"] != float64(expiry.Unix()) {
		t.Fatal("wrong endpoint claims")
	}
	return token
}

func TestListenAndControlRenewIssueEndpointTokens(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.send(map[string]string{"op": "listen", "grant": h.grantFor(7, 9, grant.ActionSeed, time.Minute)})
	reply := c.recv()
	endpoint, _ := reply["endpoint"].(string)
	public := h.relay.cfg.EndpointSigningKey.Public().(ed25519.PublicKey)
	first := assertEndpointToken(t, reply, public, endpoint, 7, 9, start.Add(time.Minute))
	h.clock.Advance(30 * time.Second)
	c.send(map[string]string{"op": "renew", "grant": h.grantFor(7, 9, grant.ActionSeed, 2*time.Minute)})
	reply = c.recv()
	if reply["op"] != "renewed" {
		t.Fatal("renew refused")
	}
	renewed := assertEndpointToken(t, reply, public, endpoint, 7, 9, h.clock.Now().Add(2*time.Minute))
	if renewed == first {
		t.Fatal("renew did not issue a fresh token")
	}
	// Even an older accepted grant must be attested at its own expiry, not the stored maximum.
	c.send(map[string]string{"op": "renew", "grant": h.grantFor(7, 9, grant.ActionSeed, 10*time.Second)})
	assertEndpointToken(t, c.recv(), public, endpoint, 7, 9, h.clock.Now().Add(10*time.Second))
	for _, value := range []string{first, renewed} {
		if strings.Contains(h.logs.String(), value) {
			t.Fatal("token leaked to logs")
		}
	}
}
