// Package relay splices TCP streams between ArkNet Nodes so that neither
// learns the other's address (docs/private-relay-design.md, option A).
//
// Wire protocol, after TLS: the client sends one JSON line (at most
// HeaderLimit bytes) and the relay answers with JSON lines.
//
//	{"op":"listen","grant":SEED}            -> {"endpoint":"100.x.y.z:p","endpoint_token":T,"expires_at":N}
//	    then the connection is the seeder's control channel:
//	    relay -> {"op":"incoming","session":ID}
//	    seeder -> {"op":"renew","grant":SEED}  -> {"op":"renewed","endpoint_token":T,"expires_at":N}
//	{"op":"connect","endpoint":E,"grant":DOWNLOAD} -> {"ok":true} then raw bytes
//	{"op":"accept","session":ID,"grant":SEED}      -> {"ok":true} then raw bytes
//	{"op":"renew","grant":G}                       -> {"op":"renewed","expires_at":N}
//
// Errors are {"error":"unauthorized"|"no_route"|"unavailable"}, after which
// the relay closes the connection.
//
// Grants are the Core-signed tracker grants. A session lasts while both
// sides hold an unexpired grant for its info hash; a later grant presented
// with "renew" extends it. Nothing is persisted, and logs carry no
// addresses, grants, subjects, hashes, endpoints or session IDs.
package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"arknet.local/relay/internal/endpointtoken"
	"arknet.local/relay/internal/grant"
)

const (
	// HeaderLimit bounds every JSON line a client sends.
	HeaderLimit = 4096
	// ClockSkew is tolerated between Core's clock and this relay's.
	ClockSkew    = 60 * time.Second
	writeTimeout = 5 * time.Second
)

type Config struct {
	GrantKey                  ed25519.PublicKey
	EndpointSigningKey        ed25519.PrivateKey
	MaxGrantLifetime          time.Duration // Core's grant lifetime (5m)
	MaxConnections            int
	MaxPendingPerRegistration int
	HeaderTimeout             time.Duration
	AcceptTimeout             time.Duration
	SweepInterval             time.Duration
	Now                       func() time.Time
	Logger                    *slog.Logger
}

type Stats struct {
	Connections, Registrations, Sessions, Pending, Revoked int
	RefusedOverLimit, Refused, SessionsTotal               int64
	BytesToSeeders, BytesToDownloaders                     int64
}

type authKey struct {
	subject [16]byte
	hash    [32]byte
	action  byte
}

type registration struct {
	endpoint string
	key      authKey
	ctrl     *trackedConn
	writeMu  sync.Mutex
	pending  int
}

func (g *registration) send(v any) error {
	g.writeMu.Lock()
	defer g.writeMu.Unlock()
	return writeJSON(g.ctrl, v)
}

type stream struct {
	conn *trackedConn
	r    *bufio.Reader
}

type pendingSession struct {
	reg *registration
	ch  chan stream
}

type session struct {
	down, seed authKey
	conns      [2]*trackedConn
}

type Relay struct {
	cfg Config

	mu        sync.Mutex
	auth      map[authKey]time.Time // latest verified grant expiry
	byEnd     map[string]*registration
	byKey     map[authKey]*registration
	pending   map[string]*pendingSession
	sessions  map[*session]struct{}
	revoked   map[[16]byte]time.Time // subject -> revocation time
	conns     map[*trackedConn]struct{}
	refusedOL atomic.Int64
	refused   atomic.Int64
	total     atomic.Int64
	toSeed    atomic.Int64
	toDown    atomic.Int64
}

func New(cfg Config) *Relay {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if cfg.MaxPendingPerRegistration <= 0 {
		cfg.MaxPendingPerRegistration = 64
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = time.Second
	}
	return &Relay{cfg: cfg, auth: map[authKey]time.Time{}, byEnd: map[string]*registration{},
		byKey: map[authKey]*registration{}, pending: map[string]*pendingSession{},
		sessions: map[*session]struct{}{}, revoked: map[[16]byte]time.Time{}, conns: map[*trackedConn]struct{}{}}
}

// TLSConfig is the relay listener's TLS policy: TLS 1.3 only.
func TLSConfig(certificate tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}
}

// trackedConn counts toward MaxConnections until it is closed.
type trackedConn struct {
	net.Conn
	relay *Relay
	once  sync.Once
}

func (c *trackedConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.Conn.Close()
		c.relay.mu.Lock()
		delete(c.relay.conns, c)
		c.relay.mu.Unlock()
	})
	return err
}

// Serve accepts connections until ctx ends, then closes every connection.
func (r *Relay) Serve(ctx context.Context, listener net.Listener) error {
	go func() { <-ctx.Done(); listener.Close() }()
	go r.sweeper(ctx)
	r.cfg.Logger.Info("relay_serving")
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				r.closeAll()
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			r.closeAll()
			return err
		}
		r.mu.Lock()
		if len(r.conns) >= r.cfg.MaxConnections {
			r.mu.Unlock()
			conn.Close()
			r.refusedOL.Add(1)
			continue
		}
		tracked := &trackedConn{Conn: conn, relay: r}
		r.conns[tracked] = struct{}{}
		r.mu.Unlock()
		go r.handle(tracked)
	}
}

func (r *Relay) closeAll() {
	r.mu.Lock()
	all := make([]*trackedConn, 0, len(r.conns))
	for c := range r.conns {
		all = append(all, c)
	}
	r.mu.Unlock()
	for _, c := range all {
		c.Close()
	}
}

func (r *Relay) sweeper(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.SweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Sweep()
		}
	}
}

// Sweep closes sessions and registrations whose grants have expired and
// forgets revocations that no unexpired grant can predate.
func (r *Relay) Sweep() {
	now := r.cfg.Now()
	var closing []*trackedConn
	expired := 0
	r.mu.Lock()
	for s := range r.sessions {
		if !now.Before(r.deadlineLocked(s)) {
			closing = append(closing, s.conns[0], s.conns[1])
			delete(r.sessions, s)
			expired++
		}
	}
	for end, g := range r.byEnd {
		if !now.Before(r.auth[g.key]) {
			closing = append(closing, g.ctrl)
			r.removeRegistrationLocked(end, g)
		}
	}
	for k, expires := range r.auth {
		if !now.Before(expires) {
			delete(r.auth, k)
		}
	}
	for subject, at := range r.revoked {
		if now.After(at.Add(r.cfg.MaxGrantLifetime + ClockSkew)) {
			delete(r.revoked, subject)
		}
	}
	r.mu.Unlock()
	for _, c := range closing {
		c.Close()
	}
	if expired > 0 {
		r.cfg.Logger.Info("relay_sessions_expired", "count", expired)
	}
}

func (r *Relay) deadlineLocked(s *session) time.Time {
	down, seed := r.auth[s.down], r.auth[s.seed]
	if seed.Before(down) {
		return seed
	}
	return down
}

func (r *Relay) removeRegistrationLocked(end string, g *registration) {
	delete(r.byEnd, end)
	if r.byKey[g.key] == g {
		delete(r.byKey, g.key)
	}
}

// Revoke closes every registration and session of the given subjects and
// refuses grants issued before now until they could have expired.
func (r *Relay) Revoke(subjects [][16]byte) int {
	now := r.cfg.Now()
	set := make(map[[16]byte]bool, len(subjects))
	for _, s := range subjects {
		set[s] = true
	}
	var closing []*trackedConn
	r.mu.Lock()
	for s := range set {
		if now.After(r.revoked[s]) {
			r.revoked[s] = now
		}
	}
	for end, g := range r.byEnd {
		if set[g.key.subject] {
			closing = append(closing, g.ctrl)
			r.removeRegistrationLocked(end, g)
		}
	}
	for s := range r.sessions {
		if set[s.down.subject] || set[s.seed.subject] {
			closing = append(closing, s.conns[0], s.conns[1])
			delete(r.sessions, s)
		}
	}
	for k := range r.auth {
		if set[k.subject] {
			delete(r.auth, k)
		}
	}
	r.mu.Unlock()
	for _, c := range closing {
		c.Close()
	}
	r.cfg.Logger.Info("relay_revocation_applied", "subjects", len(set), "connections_closed", len(closing))
	return len(closing)
}

func (r *Relay) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{Connections: len(r.conns), Registrations: len(r.byEnd), Sessions: len(r.sessions),
		Pending: len(r.pending), Revoked: len(r.revoked), RefusedOverLimit: r.refusedOL.Load(),
		Refused: r.refused.Load(), SessionsTotal: r.total.Load(), BytesToSeeders: r.toSeed.Load(),
		BytesToDownloaders: r.toDown.Load()}
}

// authorize verifies a grant for action and records its expiry.
func (r *Relay) authorize(token string, action byte) (grant.Grant, authKey, bool) {
	now := r.cfg.Now()
	g, err := grant.Verify(token, r.cfg.GrantKey, action, now)
	if err != nil || g.Expires.After(now.Add(r.cfg.MaxGrantLifetime+ClockSkew)) {
		return grant.Grant{}, authKey{}, false
	}
	key := authKey{subject: g.Subject, hash: g.Hash, action: action}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revokedLocked(g) {
		return grant.Grant{}, authKey{}, false
	}
	if g.Expires.After(r.auth[key]) {
		r.auth[key] = g.Expires
	}
	return g, key, true
}

// revokedLocked: a grant for a revoked subject that could have been issued
// before the revocation (expiry within one grant lifetime of it) is refused.
func (r *Relay) revokedLocked(g grant.Grant) bool {
	at, ok := r.revoked[g.Subject]
	return ok && !g.Expires.After(at.Add(r.cfg.MaxGrantLifetime+ClockSkew))
}

type request struct {
	Op       string `json:"op"`
	Grant    string `json:"grant"`
	Endpoint string `json:"endpoint"`
	Session  string `json:"session"`
}

func readRequest(br *bufio.Reader) (request, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		return request{}, err
	}
	var req request
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return request{}, err
	}
	return req, nil
}

func writeJSON(conn net.Conn, v any) error {
	b, _ := json.Marshal(v)
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := conn.Write(append(b, '\n'))
	_ = conn.SetWriteDeadline(time.Time{})
	return err
}

func (r *Relay) refuse(conn *trackedConn, reason string) {
	r.refused.Add(1)
	_ = writeJSON(conn, map[string]string{"error": reason})
	conn.Close()
	r.cfg.Logger.Info("relay_refused", "reason", reason)
}

func (r *Relay) handle(conn *trackedConn) {
	_ = conn.SetReadDeadline(time.Now().Add(r.cfg.HeaderTimeout))
	br := bufio.NewReaderSize(conn, HeaderLimit)
	req, err := readRequest(br)
	if err != nil {
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch req.Op {
	case "listen":
		r.serveListen(conn, br, req)
	case "connect":
		r.serveConnect(conn, br, req)
	case "accept":
		r.serveAccept(conn, br, req)
	case "renew":
		r.serveRenew(conn, req)
	default:
		conn.Close()
	}
}

func randomEndpoint() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	ip := net.IPv4(100, 64|(b[0]&0x3f), b[1], b[2])
	port := 1024 + int(binary.BigEndian.Uint16(b[3:5]))%(65536-1024)
	return net.JoinHostPort(ip.String(), strconv.Itoa(port))
}

func (r *Relay) serveListen(conn *trackedConn, br *bufio.Reader, req request) {
	g, key, ok := r.authorize(req.Grant, grant.ActionSeed)
	if !ok {
		r.refuse(conn, "unauthorized")
		return
	}
	reg := &registration{key: key, ctrl: conn}
	r.mu.Lock()
	for {
		reg.endpoint = randomEndpoint()
		if r.byEnd[reg.endpoint] == nil {
			break
		}
	}
	token, err := endpointtoken.Sign(r.cfg.EndpointSigningKey, reg.endpoint, g.Subject, g.Hash, g.Expires)
	if err != nil {
		r.mu.Unlock()
		r.refuse(conn, "unavailable")
		return
	}
	// One registration per subject and swarm: a new one replaces the old.
	var replaced *trackedConn
	if old := r.byKey[key]; old != nil {
		replaced = old.ctrl
		r.removeRegistrationLocked(old.endpoint, old)
	}
	r.byEnd[reg.endpoint] = reg
	r.byKey[key] = reg
	r.mu.Unlock()
	if replaced != nil {
		replaced.Close()
	}
	if reg.send(map[string]any{"endpoint": reg.endpoint, "endpoint_token": token, "expires_at": g.Expires.Unix()}) != nil {
		r.dropRegistration(reg)
		return
	}
	for {
		next, err := readRequest(br)
		if err != nil || next.Op != "renew" {
			break
		}
		renewed, renewedKey, ok := r.authorize(next.Grant, grant.ActionSeed)
		if !ok || renewedKey != key {
			_ = reg.send(map[string]string{"error": "unauthorized"})
			continue
		}
		token, err := endpointtoken.Sign(r.cfg.EndpointSigningKey, reg.endpoint, renewed.Subject, renewed.Hash, renewed.Expires)
		if err != nil {
			_ = reg.send(map[string]string{"error": "unavailable"})
			break
		}
		if reg.send(map[string]any{"op": "renewed", "endpoint_token": token, "expires_at": renewed.Expires.Unix()}) != nil {
			break
		}
	}
	r.dropRegistration(reg)
}

func (r *Relay) dropRegistration(reg *registration) {
	r.mu.Lock()
	if r.byEnd[reg.endpoint] == reg {
		r.removeRegistrationLocked(reg.endpoint, reg)
	}
	r.mu.Unlock()
	reg.ctrl.Close()
}

func (r *Relay) serveRenew(conn *trackedConn, req request) {
	defer conn.Close()
	var g grant.Grant
	ok := false
	for _, action := range []byte{grant.ActionDownload, grant.ActionSeed} {
		if g, _, ok = r.authorize(req.Grant, action); ok {
			break
		}
	}
	if !ok {
		r.refuse(conn, "unauthorized")
		return
	}
	_ = writeJSON(conn, map[string]any{"op": "renewed", "expires_at": g.Expires.Unix()})
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (r *Relay) serveConnect(conn *trackedConn, br *bufio.Reader, req request) {
	g, downKey, ok := r.authorize(req.Grant, grant.ActionDownload)
	if !ok {
		r.refuse(conn, "unauthorized")
		return
	}
	r.mu.Lock()
	reg := r.byEnd[req.Endpoint]
	if reg == nil || reg.key.hash != g.Hash {
		r.mu.Unlock()
		r.refuse(conn, "no_route")
		return
	}
	if reg.pending >= r.cfg.MaxPendingPerRegistration {
		r.mu.Unlock()
		r.refuse(conn, "unavailable")
		return
	}
	id := randomID()
	p := &pendingSession{reg: reg, ch: make(chan stream, 1)}
	r.pending[id] = p
	reg.pending++
	r.mu.Unlock()

	var seeder stream
	notified := reg.send(map[string]string{"op": "incoming", "session": id}) == nil
	timer := time.NewTimer(r.cfg.AcceptTimeout)
	accepted := false
	if notified {
		select {
		case seeder = <-p.ch:
			accepted = true
		case <-timer.C:
		}
	}
	timer.Stop()
	r.mu.Lock()
	delete(r.pending, id)
	reg.pending--
	if !accepted {
		// A late accept may have arrived after the timeout.
		var late *trackedConn
		select {
		case seeder = <-p.ch:
			late = seeder.conn
		default:
		}
		r.mu.Unlock()
		if late != nil {
			late.Close()
		}
		r.refuse(conn, "unavailable")
		return
	}
	s := &session{down: downKey, seed: reg.key, conns: [2]*trackedConn{conn, seeder.conn}}
	// Re-check under the lock: a revocation or expiry may have happened meanwhile.
	if r.byEnd[reg.endpoint] != reg || !r.cfg.Now().Before(r.deadlineLocked(s)) {
		r.mu.Unlock()
		seeder.conn.Close()
		r.refuse(conn, "unavailable")
		return
	}
	r.sessions[s] = struct{}{}
	r.mu.Unlock()
	r.total.Add(1)
	if writeJSON(seeder.conn, map[string]bool{"ok": true}) != nil || writeJSON(conn, map[string]bool{"ok": true}) != nil {
		r.endSession(s)
		return
	}
	r.splice(s, stream{conn, br}, seeder)
}

func (r *Relay) serveAccept(conn *trackedConn, br *bufio.Reader, req request) {
	g, key, ok := r.authorize(req.Grant, grant.ActionSeed)
	r.mu.Lock()
	p := r.pending[req.Session]
	valid := ok && p != nil && key == p.reg.key && g.Hash == p.reg.key.hash
	if valid {
		delete(r.pending, req.Session) // single use
	}
	r.mu.Unlock()
	if !valid {
		r.refuse(conn, "unauthorized")
		return
	}
	p.ch <- stream{conn, br} // buffered; the connect side owns the stream now
}

func (r *Relay) endSession(s *session) {
	r.mu.Lock()
	delete(r.sessions, s)
	r.mu.Unlock()
	s.conns[0].Close()
	s.conns[1].Close()
}

func (r *Relay) splice(s *session, down, seed stream) {
	var wg sync.WaitGroup
	copyHalf := func(dst *trackedConn, src *bufio.Reader, counter *atomic.Int64) {
		defer wg.Done()
		n, _ := io.CopyBuffer(dst, src, make([]byte, 64<<10))
		counter.Add(n)
		r.endSession(s)
	}
	wg.Add(2)
	go copyHalf(seed.conn, down.r, &r.toSeed)
	go copyHalf(down.conn, seed.r, &r.toDown)
	wg.Wait()
}
