package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet.local/relay/internal/grant"
)

var start = time.Unix(1_800_000_000, 0)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type harness struct {
	t       *testing.T
	relay   *Relay
	addr    string
	private ed25519.PrivateKey
	clock   *clock
	logs    *syncBuffer
}

func newHarness(t *testing.T, tune func(*Config)) *harness {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, private: private, clock: &clock{now: start}, logs: &syncBuffer{}}
	_, endpointKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{EndpointSigningKey: endpointKey, GrantKey: public, MaxGrantLifetime: 5 * time.Minute, MaxConnections: 64,
		HeaderTimeout: 2 * time.Second, AcceptTimeout: 2 * time.Second, SweepInterval: time.Hour,
		Now: h.clock.Now, Logger: slog.New(slog.NewJSONHandler(h.logs, nil))}
	if tune != nil {
		tune(&cfg)
	}
	h.relay = New(cfg)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.relay.Serve(ctx, listener); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return h
}

// grantFor signs a tracker-format grant for subject s and hash byte x.
func (h *harness) grantFor(s, x byte, action byte, ttl time.Duration) string {
	payload := make([]byte, grant.PayloadSize)
	payload[0] = grant.Version
	binary.BigEndian.PutUint64(payload[1:9], uint64(h.clock.Now().Add(ttl).Unix()))
	for i := 9; i < 25; i++ {
		payload[i] = s
	}
	for i := 25; i < 57; i++ {
		payload[i] = x
	}
	payload[57] = action
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(h.private, payload))
}

func subject(s byte) [16]byte { return [16]byte{s, s, s, s, s, s, s, s, s, s, s, s, s, s, s, s} }

type client struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (h *harness) dial() *client {
	h.t.Helper()
	conn, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { conn.Close() })
	return &client{t: h.t, conn: conn, r: bufio.NewReader(conn)}
}

func (c *client) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// recv returns the next message, or nil if the relay closed the connection.
func (c *client) recv() map[string]any {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(line, &m) != nil {
		c.t.Fatalf("bad message %q", line)
	}
	return m
}

// closed reports whether the relay closed the connection within 2s. A read
// timeout means it is still open.
func (c *client) closed() bool {
	_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.r.ReadByte()
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

type seeder struct {
	control  *client
	endpoint string
	streams  chan *client
	gone     chan struct{} // closed when the relay closes the control channel
}

// controlClosed reports whether the relay closed the control channel within 2s.
func (s *seeder) controlClosed() bool {
	select {
	case <-s.gone:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// controlOpen reports that the control channel is still open after a short wait.
func (s *seeder) controlOpen() bool {
	select {
	case <-s.gone:
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

// seed registers a listener and answers every incoming session.
func (h *harness) seed(token string) *seeder {
	h.t.Helper()
	control := h.dial()
	control.send(map[string]string{"op": "listen", "grant": token})
	reply := control.recv()
	endpoint, _ := reply["endpoint"].(string)
	if endpoint == "" {
		h.t.Fatalf("registration refused: %v", reply)
	}
	s := &seeder{control: control, endpoint: endpoint, streams: make(chan *client, 8), gone: make(chan struct{})}
	go func() {
		defer close(s.gone)
		for {
			_ = control.conn.SetReadDeadline(time.Time{})
			line, err := control.r.ReadBytes('\n')
			if err != nil {
				return
			}
			var m map[string]string
			_ = json.Unmarshal(line, &m)
			if m["op"] != "incoming" {
				continue
			}
			conn, err := net.Dial("tcp", h.addr)
			if err != nil {
				return
			}
			stream := &client{t: h.t, conn: conn, r: bufio.NewReader(conn)}
			b, _ := json.Marshal(map[string]string{"op": "accept", "session": m["session"], "grant": token})
			_, _ = conn.Write(append(b, '\n'))
			s.streams <- stream
		}
	}()
	return s
}

func (h *harness) connect(endpoint, token string) (*client, map[string]any) {
	h.t.Helper()
	c := h.dial()
	c.send(map[string]string{"op": "connect", "endpoint": endpoint, "grant": token})
	return c, c.recv()
}

// session opens a downloader session to s and returns both raw ends.
func (h *harness) session(s *seeder, token string) (down, up *client) {
	h.t.Helper()
	down, reply := h.connect(s.endpoint, token)
	if reply["ok"] != true {
		h.t.Fatalf("connect refused: %v", reply)
	}
	select {
	case up = <-s.streams:
	case <-time.After(3 * time.Second):
		h.t.Fatal("seeder never got the session")
	}
	if m := up.recv(); m["ok"] != true {
		h.t.Fatalf("accept refused: %v", m)
	}
	return down, up
}

func exchange(t *testing.T, down, up *client) {
	t.Helper()
	if _, err := down.conn.Write([]byte("request\n")); err != nil {
		t.Fatal(err)
	}
	_ = up.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if line, err := up.r.ReadString('\n'); err != nil || line != "request\n" {
		t.Fatalf("seeder read %q %v", line, err)
	}
	if _, err := up.conn.Write([]byte("piece\n")); err != nil {
		t.Fatal(err)
	}
	_ = down.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if line, err := down.r.ReadString('\n'); err != nil || line != "piece\n" {
		t.Fatalf("downloader read %q %v", line, err)
	}
}

func TestSeedGrantRegistersARandomVirtualEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	seen := map[string]bool{}
	for s := byte(1); s <= 3; s++ {
		endpoint := h.seed(h.grantFor(s, 9, grant.ActionSeed, time.Minute)).endpoint
		host, portText, err := net.SplitHostPort(endpoint)
		ip := net.ParseIP(host)
		port, portErr := strconv.Atoi(portText)
		_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
		if err != nil || portErr != nil || ip == nil || !cgnat.Contains(ip) || port < 1024 || port > 65535 || seen[endpoint] {
			t.Fatalf("endpoint %q", endpoint)
		}
		seen[endpoint] = true
	}
}

func TestDownloadGrantCannotRegisterAListener(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.send(map[string]string{"op": "listen", "grant": h.grantFor(1, 9, grant.ActionDownload, time.Minute)})
	if m := c.recv(); m["error"] != "unauthorized" || !c.closed() {
		t.Fatalf("%v", m)
	}
}

func TestDownloaderReachesSeederOnlyThroughItsEndpoint(t *testing.T) {
	h := newHarness(t, nil)
	s := h.seed(h.grantFor(1, 9, grant.ActionSeed, time.Minute))
	down, up := h.session(s, h.grantFor(2, 9, grant.ActionDownload, time.Minute))
	exchange(t, down, up)
	if st := h.relay.Stats(); st.Sessions != 1 || st.Registrations != 1 {
		t.Fatalf("%+v", st)
	}
	down.conn.Close()
	if !up.closed() {
		t.Fatal("seeder side stayed open after the downloader left")
	}
}

func TestConnectRefusals(t *testing.T) {
	h := newHarness(t, nil)
	s := h.seed(h.grantFor(1, 9, grant.ActionSeed, time.Minute))
	for name, tc := range map[string]struct{ endpoint, token, want string }{
		"other info hash":  {s.endpoint, h.grantFor(2, 8, grant.ActionDownload, time.Minute), "no_route"},
		"seed grant":       {s.endpoint, h.grantFor(2, 9, grant.ActionSeed, time.Minute), "unauthorized"},
		"expired grant":    {s.endpoint, h.grantFor(2, 9, grant.ActionDownload, -time.Second), "unauthorized"},
		"forged grant":     {s.endpoint, h.grantFor(2, 9, grant.ActionDownload, time.Minute)[:40] + "AAAA" + h.grantFor(2, 9, grant.ActionDownload, time.Minute)[44:], "unauthorized"},
		"unknown endpoint": {"100.64.0.1:4000", h.grantFor(2, 9, grant.ActionDownload, time.Minute), "no_route"},
		"not an endpoint":  {"10.0.0.1:6881", h.grantFor(2, 9, grant.ActionDownload, time.Minute), "no_route"},
	} {
		c, m := h.connect(tc.endpoint, tc.token)
		if m["error"] != tc.want || !c.closed() {
			t.Errorf("%s: %v", name, m)
		}
	}
	select {
	case <-s.streams:
		t.Fatal("a refused connect reached the seeder")
	default:
	}
}

func TestAcceptRequiresTheRegisteredSeeder(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AcceptTimeout = 300 * time.Millisecond })
	seedToken := h.grantFor(1, 9, grant.ActionSeed, time.Minute)
	control := h.dial()
	control.send(map[string]string{"op": "listen", "grant": seedToken})
	endpoint := control.recv()["endpoint"].(string)
	down := h.dial()
	down.send(map[string]string{"op": "connect", "endpoint": endpoint, "grant": h.grantFor(2, 9, grant.ActionDownload, time.Minute)})
	incoming := control.recv()
	if incoming["op"] != "incoming" || len(incoming) != 2 {
		t.Fatalf("incoming notice carries more than a session ID: %v", incoming)
	}
	intruder := h.dial()
	intruder.send(map[string]any{"op": "accept", "session": incoming["session"], "grant": h.grantFor(3, 9, grant.ActionSeed, time.Minute)})
	if m := intruder.recv(); m["error"] != "unauthorized" {
		t.Fatalf("another subject accepted the session: %v", m)
	}
	if m := down.recv(); m["error"] != "unavailable" {
		t.Fatalf("downloader: %v", m)
	}
	late := h.dial()
	late.send(map[string]any{"op": "accept", "session": incoming["session"], "grant": seedToken})
	if m := late.recv(); m["error"] != "unauthorized" {
		t.Fatalf("expired session accepted: %v", m)
	}
}

func TestSessionIDsAreSingleUse(t *testing.T) {
	h := newHarness(t, nil)
	seedToken := h.grantFor(1, 9, grant.ActionSeed, time.Minute)
	control := h.dial()
	control.send(map[string]string{"op": "listen", "grant": seedToken})
	endpoint := control.recv()["endpoint"].(string)
	down := h.dial()
	down.send(map[string]string{"op": "connect", "endpoint": endpoint, "grant": h.grantFor(2, 9, grant.ActionDownload, time.Minute)})
	id := control.recv()["session"]
	first := h.dial()
	first.send(map[string]any{"op": "accept", "session": id, "grant": seedToken})
	if m := first.recv(); m["ok"] != true {
		t.Fatalf("first accept: %v", m)
	}
	second := h.dial()
	second.send(map[string]any{"op": "accept", "session": id, "grant": seedToken})
	if m := second.recv(); m["error"] != "unauthorized" {
		t.Fatalf("session ID accepted twice: %v", m)
	}
}

func TestGrantExpiryClosesLiveSessionsAndRegistrations(t *testing.T) {
	h := newHarness(t, nil)
	s := h.seed(h.grantFor(1, 9, grant.ActionSeed, 2*time.Minute))
	down, up := h.session(s, h.grantFor(2, 9, grant.ActionDownload, time.Minute))
	exchange(t, down, up)
	h.clock.Advance(59 * time.Second)
	h.relay.Sweep()
	exchange(t, down, up) // still inside both grants
	h.clock.Advance(time.Second)
	h.relay.Sweep()
	if !down.closed() || !up.closed() {
		t.Fatal("session outlived the download grant")
	}
	if !s.controlOpen() {
		t.Fatal("registration closed before its own grant expired")
	}
	h.clock.Advance(time.Minute)
	h.relay.Sweep()
	if !s.controlClosed() || h.relay.Stats().Registrations != 0 {
		t.Fatal("registration outlived its seed grant")
	}
}

func TestRenewalExtendsSessionsAndRegistrations(t *testing.T) {
	h := newHarness(t, nil)
	s := h.seed(h.grantFor(1, 9, grant.ActionSeed, time.Minute))
	down, up := h.session(s, h.grantFor(2, 9, grant.ActionDownload, time.Minute))
	h.clock.Advance(50 * time.Second)
	// The seeder renews on its control channel; the downloader with a short renew request.
	s.control.send(map[string]string{"op": "renew", "grant": h.grantFor(1, 9, grant.ActionSeed, time.Minute)})
	renew := h.dial()
	renew.send(map[string]string{"op": "renew", "grant": h.grantFor(2, 9, grant.ActionDownload, time.Minute)})
	if m := renew.recv(); m["op"] != "renewed" {
		t.Fatalf("downloader renewal: %v", m)
	}
	time.Sleep(100 * time.Millisecond) // the control-channel renewal is processed asynchronously
	h.clock.Advance(20 * time.Second)  // past both original grants
	h.relay.Sweep()
	exchange(t, down, up)
	if h.relay.Stats().Registrations != 1 {
		t.Fatal("renewed registration dropped")
	}
	// A renewal for another subject or hash extends nothing.
	other := h.dial()
	other.send(map[string]string{"op": "renew", "grant": h.grantFor(2, 8, grant.ActionDownload, 5*time.Minute)})
	other.recv()
	h.clock.Advance(41 * time.Second)
	h.relay.Sweep()
	if !down.closed() {
		t.Fatal("a renewal for another info hash extended the session")
	}
}

func TestGrantsBeyondTheMaximumLifetimeAreRefused(t *testing.T) {
	h := newHarness(t, nil)
	c := h.dial()
	c.send(map[string]string{"op": "listen", "grant": h.grantFor(1, 9, grant.ActionSeed, 5*time.Minute+ClockSkew+time.Second)})
	if m := c.recv(); m["error"] != "unauthorized" {
		t.Fatalf("over-long grant accepted: %v", m)
	}
}

func TestRevocationClosesSessionsAndRefusesReconnects(t *testing.T) {
	h := newHarness(t, nil)
	seedToken := h.grantFor(1, 9, grant.ActionSeed, 5*time.Minute)
	downToken := h.grantFor(2, 9, grant.ActionDownload, 5*time.Minute)
	s := h.seed(seedToken)
	down, up := h.session(s, downToken)
	bystander := h.seed(h.grantFor(3, 7, grant.ActionSeed, 5*time.Minute))
	otherDown, otherUp := h.session(bystander, h.grantFor(4, 7, grant.ActionDownload, 5*time.Minute))

	if closed := h.relay.Revoke([][16]byte{subject(2)}); closed == 0 {
		t.Fatal("nothing closed")
	}
	if !down.closed() || !up.closed() {
		t.Fatal("revoked downloader's session stayed open")
	}
	exchange(t, otherDown, otherUp)
	if c, m := h.connect(s.endpoint, downToken); m["error"] != "unauthorized" || !c.closed() {
		t.Fatalf("revoked downloader reconnected: %v", m)
	}
	renew := h.dial()
	renew.send(map[string]string{"op": "renew", "grant": downToken})
	if m := renew.recv(); m["error"] != "unauthorized" {
		t.Fatalf("revoked downloader renewed: %v", m)
	}

	h.relay.Revoke([][16]byte{subject(1)})
	if !s.controlClosed() || h.relay.Stats().Registrations != 1 {
		t.Fatal("revoked seeder's registration stayed")
	}
	again := h.dial()
	again.send(map[string]string{"op": "listen", "grant": seedToken})
	if m := again.recv(); m["error"] != "unauthorized" {
		t.Fatalf("revoked seeder re-registered: %v", m)
	}

	// Grants issued before the revocation stay refused until they could have
	// expired; after that the record is dropped (a later grant would be new).
	h.clock.Advance(5*time.Minute + ClockSkew + time.Second)
	h.relay.Sweep()
	if h.relay.Stats().Revoked != 0 {
		t.Fatal("revocation records kept forever")
	}
	h.seed(h.grantFor(1, 9, grant.ActionSeed, time.Minute))
}

func TestHeaderSizeAndTimeoutAreBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.HeaderTimeout = 300 * time.Millisecond })
	big := h.dial()
	_, _ = big.conn.Write([]byte(strings.Repeat("x", HeaderLimit+1)))
	if !big.closed() {
		t.Fatal("oversized header kept open")
	}
	idle := h.dial()
	if !idle.closed() {
		t.Fatal("silent connection kept open")
	}
	junk := h.dial()
	junk.send(map[string]string{"op": "teleport"})
	if !junk.closed() {
		t.Fatal("unknown operation kept open")
	}
}

func TestConnectionCountIsBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxConnections = 3; c.HeaderTimeout = 5 * time.Second })
	for i := 0; i < 3; i++ {
		h.dial()
	}
	time.Sleep(100 * time.Millisecond)
	if extra := h.dial(); !extra.closed() {
		t.Fatal("connection beyond the limit accepted")
	}
	if h.relay.Stats().RefusedOverLimit == 0 {
		t.Fatal("refusal not counted")
	}
}

func TestPendingSessionsPerRegistrationAreBounded(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxPendingPerRegistration = 2; c.AcceptTimeout = 10 * time.Second })
	control := h.dial()
	control.send(map[string]string{"op": "listen", "grant": h.grantFor(1, 9, grant.ActionSeed, time.Minute)})
	endpoint := control.recv()["endpoint"].(string)
	for i := 0; i < 2; i++ {
		c := h.dial()
		c.send(map[string]string{"op": "connect", "endpoint": endpoint, "grant": h.grantFor(byte(10+i), 9, grant.ActionDownload, time.Minute)})
	}
	time.Sleep(200 * time.Millisecond)
	if _, m := h.connect(endpoint, h.grantFor(20, 9, grant.ActionDownload, time.Minute)); m["error"] != "unavailable" {
		t.Fatalf("pending sessions unbounded: %v", m)
	}
}

func TestTLSConfigRequiresTLS13(t *testing.T) {
	cfg := TLSConfig(tls.Certificate{})
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("min version %x", cfg.MinVersion)
	}
}

func TestLogsCarryNoAddressesGrantsSubjectsHashesOrEndpoints(t *testing.T) {
	h := newHarness(t, nil)
	seedToken := h.grantFor(0x5a, 0x6b, grant.ActionSeed, time.Minute)
	downToken := h.grantFor(0x7c, 0x6b, grant.ActionDownload, time.Minute)
	s := h.seed(seedToken)
	down, up := h.session(s, downToken)
	exchange(t, down, up)
	h.connect(s.endpoint, h.grantFor(0x7c, 0x11, grant.ActionDownload, time.Minute))
	h.connect("100.64.0.1:4000", downToken)
	h.relay.Revoke([][16]byte{subject(0x7c)})
	h.clock.Advance(2 * time.Minute)
	h.relay.Sweep()
	logs := h.logs.String()
	if !strings.Contains(logs, "relay_") {
		t.Fatalf("no relay events logged, so the check proves nothing: %q", logs)
	}
	host, _, _ := net.SplitHostPort(s.endpoint)
	seedSubject, downSubject := subject(0x5a), subject(0x7c)
	forbidden := []string{seedToken, downToken, seedToken[:20], s.endpoint, host, "127.0.0.1", down.conn.LocalAddr().String(),
		hex.EncodeToString(seedSubject[:]), hex.EncodeToString(downSubject[:]),
		strings.Repeat("6b", 32), base64.StdEncoding.EncodeToString(seedSubject[:])}
	for _, value := range forbidden {
		if strings.Contains(logs, value) {
			t.Errorf("log contains %q", value)
		}
	}
}
