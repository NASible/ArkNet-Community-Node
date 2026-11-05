// THROWAWAY protocol prototype. No product API, protocol, or implementation reuse.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const aip = "127.0.0.2"
const bip = "127.0.0.3"
const cip = "127.0.0.4"
const oip = "127.0.0.5"
const canary = "SPIKE_GRANT_CANARY_DO_NOT_DISCLOSE"

type claims struct {
	Kind     string
	ID       string
	Audience string
	SPKI     string
	Expiry   int64
	Role     string
	Hash     string
	Route    string
	Canary   string
}
type signed struct {
	Body json.RawMessage
	Sig  []byte
}

func sign(k ed25519.PrivateKey, c claims) signed {
	b, _ := json.Marshal(c)
	return signed{b, ed25519.Sign(k, b)}
}
func verify(k ed25519.PublicKey, s signed, kind string) (claims, error) {
	var c claims
	if !ed25519.Verify(k, s.Body, s.Sig) {
		return c, errors.New("signature")
	}
	if json.Unmarshal(s.Body, &c) != nil || c.Kind != kind || time.Now().UnixNano() >= c.Expiry {
		return c, errors.New("claims")
	}
	return c, nil
}
func frame(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > 8192 {
		return errors.New("size")
	}
	if e = binary.Write(w, binary.BigEndian, uint32(len(b))); e != nil {
		return e
	}
	_, e = w.Write(b)
	return e
}
func unframe(r io.Reader, v any) error {
	var n uint32
	if e := binary.Read(r, binary.BigEndian, &n); e != nil {
		return e
	}
	if n > 8192 {
		return errors.New("size")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(r, b); e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func spki(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(h[:])
}

type identity struct {
	cert tls.Certificate
	leaf *x509.Certificate
}

func makeID(name string, ca *x509.Certificate, key ed25519.PrivateKey) identity {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	t := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{name}}
	der, e := x509.CreateCertificate(rand.Reader, t, ca, pub, key)
	must(e)
	leaf, _ := x509.ParseCertificate(der)
	return identity{tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, leaf}
}
func must(e error) {
	if e != nil {
		panic(e)
	}
}

type countConn struct {
	net.Conn
	tx *atomic.Int64
}

func (c *countConn) Write(p []byte) (int, error) {
	n, e := c.Conn.Write(p)
	c.tx.Add(int64(n))
	return n, e
}

type hop struct {
	Owner  string
	Local  string
	Remote string
}
type capture struct {
	mu       sync.Mutex
	patterns [][]byte
	found    []bool
	tails    map[string][]byte
	total    int64
}

func (c *capture) add(p []byte) { c.stream("test", p) }
func (c *capture) stream(id string, p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	if c.tails == nil {
		c.tails = map[string][]byte{}
	}
	b := append(append([]byte{}, c.tails[id]...), p...)
	for i, v := range c.patterns {
		if bytes.Contains(b, v) {
			c.found[i] = true
		}
	}
	n := 128
	if len(b) < n {
		n = len(b)
	}
	c.tails[id] = append(c.tails[id][:0], b[len(b)-n:]...)
}

type tap struct {
	io.Reader
	c  *capture
	id string
}

func (t tap) Read(p []byte) (int, error) {
	n, e := t.Reader.Read(p)
	t.c.stream(t.id, p[:n])
	return n, e
}

type peer struct {
	c    *tls.Conn
	done chan struct{}
}
type env struct {
	core                               ed25519.PrivateKey
	pub                                ed25519.PublicKey
	admissionKey                       ed25519.PrivateKey
	admissionPub                       ed25519.PublicKey
	pool                               *x509.CertPool
	ids                                map[string]identity
	direct, outer, community           net.Listener
	opTX, cTX, appTX, auth, grantsSent atomic.Int64
	opDirectTX, opOuterTX              atomic.Int64
	mu                                 sync.Mutex
	admission                          signed
	revoked                            bool
	active                             map[net.Conn]bool
	hops                               []hop
	peers                              map[string]peer
	cap                                capture
	hash                               string
	hashRaw                            []byte
	size                               int64
	wg                                 sync.WaitGroup
	stop                               chan struct{}
}

func newEnv(size int64) *env {
	e := &env{size: size, ids: map[string]identity{}, active: map[net.Conn]bool{}, peers: map[string]peer{}, stop: make(chan struct{})}
	e.pub, e.core, _ = ed25519.GenerateKey(rand.Reader)
	e.admissionPub, e.admissionKey, _ = ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "throwaway-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	must(err)
	ca, _ = x509.ParseCertificate(der)
	e.pool = x509.NewCertPool()
	e.pool.AddCert(ca)
	for _, n := range []string{"A", "B", "C", "O"} {
		e.ids[n] = makeID(n, ca, key)
	}
	h := sha256.New()
	_, err = io.CopyN(h, patternReader{}, size)
	must(err)
	e.hashRaw = h.Sum(nil)
	e.hash = hex.EncodeToString(e.hashRaw)
	e.cap.patterns = [][]byte{[]byte(canary), []byte(e.hash), e.hashRaw, []byte(aip), net.ParseIP(aip).To4(), []byte(bip), net.ParseIP(bip).To4()}
	e.cap.found = make([]bool, len(e.cap.patterns))
	e.admission = e.admit(time.Now().Add(time.Minute))
	e.direct = listen(oip)
	e.outer = listen(oip)
	e.community = listen(cip)
	e.accept(e.direct, func(c net.Conn) { e.inner(c, "direct") })
	e.accept(e.outer, e.handleOuter)
	e.accept(e.community, e.handleCommunity)
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-t.C:
				e.mu.Lock()
				if !e.admittedLocked() {
					for c := range e.active {
						c.Close()
					}
				}
				e.mu.Unlock()
			}
		}
	}()
	return e
}
func listen(ip string) net.Listener { l, e := net.Listen("tcp4", ip+":0"); must(e); return l }
func (e *env) admit(exp time.Time) signed {
	return sign(e.admissionKey, claims{Kind: "admission-v1", ID: "community-C", Audience: spki(e.ids["O"].leaf), SPKI: spki(e.ids["C"].leaf), Expiry: exp.UnixNano()})
}
func (e *env) admittedLocked() bool {
	c, err := verify(e.admissionPub, e.admission, "admission-v1")
	return err == nil && !e.revoked && c.ID == "community-C" && c.Audience == spki(e.ids["O"].leaf) && c.SPKI == spki(e.ids["C"].leaf)
}
func (e *env) record(owner string, c net.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hops = append(e.hops, hop{owner, c.LocalAddr().String(), c.RemoteAddr().String()})
}
func (e *env) accept(l net.Listener, f func(net.Conn)) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			e.wg.Add(1)
			go func() { defer e.wg.Done(); defer c.Close(); c.SetDeadline(time.Now().Add(30 * time.Second)); f(c) }()
		}
	}()
}
func (e *env) server() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{e.ids["O"].cert}, ClientCAs: e.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
}
func (e *env) client(name string, bad bool) *tls.Config {
	pin := spki(e.ids["O"].leaf)
	if bad {
		pin = "wrong"
	}
	return &tls.Config{Certificates: []tls.Certificate{e.ids[name].cert}, RootCAs: e.pool, ServerName: "O", MinVersion: tls.VersionTLS13, VerifyConnection: func(s tls.ConnectionState) error {
		if spki(s.PeerCertificates[0]) != pin {
			return errors.New("pin")
		}
		return nil
	}}
}
func (e *env) handleOuter(raw net.Conn) {
	e.record("O-outer", raw)
	c := tls.Server(&countConn{&countConn{raw, &e.opOuterTX}, &e.opTX}, e.server())
	if c.Handshake() != nil {
		return
	}
	if spki(c.ConnectionState().PeerCertificates[0]) != spki(e.ids["C"].leaf) {
		return
	}
	e.mu.Lock()
	ok := e.admittedLocked()
	if ok {
		e.active[raw] = true
	}
	e.mu.Unlock()
	if !ok {
		return
	}
	defer func() { e.mu.Lock(); delete(e.active, raw); e.mu.Unlock() }()
	if _, err := c.Write([]byte{1}); err != nil {
		return
	}
	e.inner(c, "community-C")
}
func (e *env) inner(raw net.Conn, route string) {
	var base net.Conn = raw
	if route == "direct" {
		e.record("O-direct", raw)
		base = &countConn{&countConn{raw, &e.opDirectTX}, &e.opTX}
	}
	c := tls.Server(base, e.server())
	if c.Handshake() != nil {
		return
	}
	var s signed
	if unframe(c, &s) != nil {
		return
	}
	g, err := verify(e.pub, s, "grant-v1")
	name := c.ConnectionState().PeerCertificates[0].Subject.CommonName
	if err != nil || g.Audience != spki(e.ids["O"].leaf) || g.ID != name || g.Hash != e.hash || g.Route != route || g.Canary != canary || !((g.Role == "send" && name == "A") || (g.Role == "receive" && name == "B")) {
		return
	}
	e.auth.Add(1)
	if _, err = c.Write([]byte{1}); err != nil {
		return
	}
	p := peer{c, make(chan struct{})}
	e.mu.Lock()
	e.peers[g.Role] = p
	a, aok := e.peers["send"]
	b, bok := e.peers["receive"]
	if aok && bok {
		delete(e.peers, "send")
		delete(e.peers, "receive")
	}
	e.mu.Unlock()
	if aok && bok {
		n, _ := io.CopyN(b.c, a.c, e.size)
		e.appTX.Add(n)
		close(a.done)
		close(b.done)
	}
	<-p.done
}
func (e *env) dial(ip, addr string) (net.Conn, error) {
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}, Timeout: time.Second}
	return d.Dial("tcp4", addr)
}
func (e *env) handleCommunity(raw net.Conn) {
	e.record("C-node", raw)
	outer, err := e.dial(cip, e.outer.Addr().String())
	if err != nil {
		return
	}
	defer outer.Close()
	e.record("C-outer", outer)
	c := tls.Client(&countConn{outer, &e.cTX}, e.client("C", false))
	if c.Handshake() != nil {
		return
	}
	var ack [1]byte
	if _, err = io.ReadFull(c, ack[:]); err != nil {
		return
	}
	e.cap.stream(raw.RemoteAddr().String()+"-outer-control", ack[:])
	done := make(chan struct{})
	go func() { io.Copy(c, tap{raw, &e.cap, raw.RemoteAddr().String() + "-up"}); c.Close(); close(done) }()
	io.Copy(&countConn{raw, &e.cTX}, tap{c, &e.cap, raw.RemoteAddr().String() + "-down"})
	raw.Close()
	c.Close()
	<-done
}
func (e *env) connect(name string, via, bad bool, routeOverride string) (*tls.Conn, error) {
	ip := aip
	if name == "B" {
		ip = bip
	}
	addr := e.direct.Addr().String()
	route := "direct"
	if via {
		addr = e.community.Addr().String()
		route = "community-C"
	}
	if routeOverride != "" {
		route = routeOverride
	}
	raw, err := e.dial(ip, addr)
	if err != nil {
		return nil, err
	}
	raw.SetDeadline(time.Now().Add(20 * time.Second))
	e.record("Node"+name, raw)
	c := tls.Client(raw, e.client(name, bad))
	if err = c.Handshake(); err != nil {
		c.Close()
		return nil, err
	}
	g := sign(e.core, claims{Kind: "grant-v1", ID: name, Audience: spki(e.ids["O"].leaf), Expiry: time.Now().Add(time.Minute).UnixNano(), Role: map[string]string{"A": "send", "B": "receive"}[name], Hash: e.hash, Route: route, Canary: canary})
	e.grantsSent.Add(1)
	if err = frame(c, g); err == nil {
		var b [1]byte
		_, err = io.ReadFull(c, b[:])
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
func (e *env) close() {
	close(e.stop)
	e.direct.Close()
	e.outer.Close()
	e.community.Close()
	e.mu.Lock()
	for c := range e.active {
		c.Close()
	}
	for _, p := range e.peers {
		p.c.Close()
		close(p.done)
	}
	e.mu.Unlock()
	e.wg.Wait()
}

type patternReader struct{}

func (patternReader) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(i % 251)
	}
	return len(b), nil
}

// Use fixed 32KiB chunks for identical generator offset/reset at sender and verifier.
func payload(n int64) []byte {
	b := make([]byte, n)
	for start := 0; start < len(b); start += 32768 {
		end := start + 32768
		if end > len(b) {
			end = len(b)
		}
		patternReader{}.Read(b[start:end])
	}
	return b
}

type sample struct {
	Topology                     string
	Bytes                        int64
	Seconds                      float64
	MiBPerSecond                 float64
	OperatorApplicationEgress    int64
	OperatorTLSWireEgress        int64
	OperatorDirectWireEgress     int64
	OperatorOuterWireEgress      int64
	CommunityTLSWireEgress       int64
	CommunityDecryptedOuterBytes int64
	PayloadSHA256Verified        bool
	CommunitySecretScanClear     bool
	CommunityScanHits            []string
	OperatorReadGrantsAndHash    bool
	CommunityOrigins             []string
	Unsafe                       bool
	Hops                         []hop
}

func runTransfer(topology string, n int64) (sample, error) {
	e := newEnv(n)
	defer e.close()
	aVia := topology == "A-C-O-B" || topology == "A-C-O-C-B"
	bVia := topology == "A-O-C-B" || topology == "A-C-O-C-B"
	start := time.Now()
	b, err := e.connect("B", bVia, false, "")
	if err != nil {
		return sample{}, err
	}
	defer b.Close()
	a, err := e.connect("A", aVia, false, "")
	if err != nil {
		return sample{}, err
	}
	defer a.Close()
	p := payload(n)
	want := sha256.Sum256(p)
	send := make(chan error, 1)
	go func() { _, err := a.Write(p); send <- err }()
	h := sha256.New()
	got, err := io.CopyN(h, b, n)
	if err != nil {
		return sample{}, err
	}
	if err = <-send; err != nil {
		return sample{}, err
	}
	secs := time.Since(start).Seconds()
	a.Close()
	b.Close()
	time.Sleep(20 * time.Millisecond)
	e.cap.mu.Lock()
	clear := true
	hits := []string{}
	labels := []string{"grant_canary", "hash_text", "hash_binary", "A_address_text", "A_address_packed", "B_address_text", "B_address_packed"}
	for i, v := range e.cap.found {
		clear = clear && !v
		if v {
			hits = append(hits, labels[i])
		}
	}
	captured := e.cap.total
	e.cap.mu.Unlock()
	e.mu.Lock()
	hops := append([]hop(nil), e.hops...)
	e.mu.Unlock()
	origins := []string{}
	seen := map[string]bool{}
	for _, v := range hops {
		ip, _, _ := net.SplitHostPort(v.Remote)
		if v.Owner == "C-node" && !seen[ip] {
			seen[ip] = true
			origins = append(origins, ip)
		}
	}
	sort.Strings(origins)
	return sample{Topology: topology, Bytes: n, Seconds: secs, MiBPerSecond: float64(n) / (1024 * 1024) / secs, OperatorApplicationEgress: e.appTX.Load(), OperatorTLSWireEgress: e.opTX.Load(), OperatorDirectWireEgress: e.opDirectTX.Load(), OperatorOuterWireEgress: e.opOuterTX.Load(), CommunityTLSWireEgress: e.cTX.Load(), CommunityDecryptedOuterBytes: captured, PayloadSHA256Verified: got == n && bytes.Equal(want[:], h.Sum(nil)) && bytes.Equal(e.hashRaw, h.Sum(nil)), CommunitySecretScanClear: clear, CommunityScanHits: hits, OperatorReadGrantsAndHash: e.auth.Load() == 2, CommunityOrigins: origins, Unsafe: topology == "A-C-O-C-B", Hops: hops}, nil
}

type check struct {
	Name   string
	Status string
}

func result(name string, ok bool) check {
	s := "FAIL"
	if ok {
		s = "PASS"
	}
	return check{name, s}
}
func median(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n%2 == 0 {
		return (v[n/2-1] + v[n/2]) / 2
	}
	return v[n/2]
}
func validMeasurement(s sample) bool {
	if !s.PayloadSHA256Verified || !s.CommunitySecretScanClear || !s.OperatorReadGrantsAndHash || s.OperatorApplicationEgress != s.Bytes || s.OperatorTLSWireEgress < s.Bytes || s.OperatorTLSWireEgress != s.OperatorDirectWireEgress+s.OperatorOuterWireEgress {
		return false
	}
	if s.Topology == "A-O-B" {
		return len(s.CommunityOrigins) == 0 && s.CommunityDecryptedOuterBytes == 0 && s.CommunityTLSWireEgress == 0 && s.OperatorOuterWireEgress == 0
	}
	if s.CommunityDecryptedOuterBytes < s.Bytes || s.CommunityTLSWireEgress < s.Bytes || s.OperatorOuterWireEgress == 0 {
		return false
	}
	if s.Unsafe {
		return len(s.CommunityOrigins) == 2 && s.CommunityOrigins[0] == aip && s.CommunityOrigins[1] == bip
	}
	want := aip
	if s.Topology == "A-O-C-B" {
		want = bip
	}
	return len(s.CommunityOrigins) == 1 && s.CommunityOrigins[0] == want
}
func negativeChecks() []check {
	out := []check{}
	e := newEnv(1)
	_, err := e.connect("A", false, true, "")
	out = append(out, result("wrong_operator_pin_before_any_grant_sent", err != nil && e.auth.Load() == 0 && e.grantsSent.Load() == 0))
	e.close()
	for _, kind := range []string{"tampered", "expired", "revoked"} {
		e = newEnv(1)
		e.mu.Lock()
		if kind == "tampered" {
			e.admission.Sig[0] ^= 1
		}
		if kind == "expired" {
			e.admission = e.admit(time.Now().Add(-time.Second))
		}
		if kind == "revoked" {
			e.revoked = true
		}
		e.mu.Unlock()
		_, err = e.connect("A", true, false, "")
		out = append(out, result(kind+"_admission_denies_new_tunnel", err != nil && e.auth.Load() == 0))
		e.close()
	}
	e = newEnv(1)
	e.mu.Lock()
	e.revoked = true
	e.mu.Unlock()
	// Malicious revoked C re-proxies Node A's intact inner TLS to O-direct,
	// using C's actual source address and no outer admission handshake.
	e.community.Close()
	e.community = listen(cip)
	e.accept(e.community, func(node net.Conn) {
		up, de := e.dial(cip, e.direct.Addr().String())
		if de != nil {
			return
		}
		defer up.Close()
		e.record("malicious-C-direct", up)
		done := make(chan struct{})
		go func() { io.Copy(up, node); up.Close(); close(done) }()
		io.Copy(node, up)
		node.Close()
		<-done
	})
	_, err = e.connect("A", true, false, "community-C")
	out = append(out, result("revoked_C_reproxy_to_direct_listener_rejects_C_bound_grant", err != nil && e.auth.Load() == 0 && e.grantsSent.Load() == 1))
	e.close()
	for _, kind := range []string{"revoked", "expired"} {
		e = newEnv(1)
		if kind == "expired" {
			e.mu.Lock()
			e.admission = e.admit(time.Now().Add(150 * time.Millisecond))
			e.mu.Unlock()
		}
		c, ce := e.connect("A", true, false, "")
		ok := ce == nil
		if ok {
			start := time.Now()
			e.mu.Lock()
			if kind == "revoked" {
				e.revoked = true
			}
			e.mu.Unlock()
			c.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			_, re := c.Read(b[:])
			ne, timeout := re.(net.Error)
			ok = re != nil && !(timeout && ne.Timeout()) && time.Since(start) < time.Second
			c.Close()
			_, err = e.connect("B", true, false, "")
			ok = ok && err != nil
		}
		out = append(out, result(kind+"_closes_active_tunnel_within_1s_and_denies_new", ok))
		e.close()
	}
	e = newEnv(1)
	target := listen("127.0.0.6")
	accepted := make(chan bool, 1)
	go func() {
		c, err := target.Accept()
		if err == nil {
			c.Close()
			accepted <- true
		}
	}()
	c, ce := e.dial(aip, e.community.Addr().String())
	ok := ce == nil
	if ok {
		c.SetDeadline(time.Now().Add(time.Second))
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\n\r\n", target.Addr())
		var b [1]byte
		_, re := c.Read(b[:])
		ne, isNet := re.(net.Error)
		ok = re != nil && !(isNet && ne.Timeout())
		c.Close()
	}
	select {
	case <-accepted:
		ok = false
	default:
	}
	target.Close()
	out = append(out, result("community_fixed_operator_target_rejects_connect_not_open_proxy", ok))
	e.close()
	return out
}
func main() {
	mib := flag.Int64("mib", 32, "payload MiB per repetition")
	reps := flag.Int("reps", 3, "repetitions per topology")
	flag.Parse()
	if *mib < 1 || *mib > 256 || *reps < 1 {
		fmt.Fprintln(os.Stderr, "invalid flags")
		os.Exit(2)
	}
	checks := negativeChecks()
	samples := []sample{}
	medians := map[string]map[string]float64{}
	for _, top := range []string{"A-O-B", "A-C-O-B", "A-O-C-B"} {
		ss := []float64{}
		ww := []float64{}
		apps, cts, directs, outers := []float64{}, []float64{}, []float64{}, []float64{}
		for i := 0; i < *reps; i++ {
			s, err := runTransfer(top, *mib*1024*1024)
			if err != nil {
				fmt.Fprintln(os.Stderr, "transfer failed", top, err)
				os.Exit(1)
			}
			samples = append(samples, s)
			ss = append(ss, s.MiBPerSecond)
			ww = append(ww, float64(s.OperatorTLSWireEgress))
			apps = append(apps, float64(s.OperatorApplicationEgress))
			cts = append(cts, float64(s.CommunityTLSWireEgress))
			directs = append(directs, float64(s.OperatorDirectWireEgress))
			outers = append(outers, float64(s.OperatorOuterWireEgress))
			checks = append(checks, result(fmt.Sprintf("%s_rep%d_payload_capture_and_egress_conservation", top, i+1), validMeasurement(s)))
		}
		sort.Float64s(ss)
		sort.Float64s(ww)
		medians[top] = map[string]float64{"throughput_mib_s": median(ss), "operator_tls_wire_egress_bytes": median(ww), "operator_application_egress_bytes": median(apps), "community_tls_wire_egress_bytes": median(cts), "operator_direct_wire_egress_bytes": median(directs), "operator_outer_wire_egress_bytes": median(outers)}
	}
	unsafe, err := runTransfer("A-C-O-C-B", 1024*1024)
	checks = append(checks, result("same_community_both_legs_is_unsafe_observes_both_node_origins", err == nil && unsafe.Unsafe && validMeasurement(unsafe)))
	samples = append(samples, unsafe)
	// Negative control: C's measured adjacent origin plus the opposite Node's
	// measured getsockname. Knowledge union, not a traffic-correlation claim.
	collusion := []map[string]any{}
	for _, top := range []string{"A-C-O-B", "A-O-C-B"} {
		for _, s := range samples {
			if s.Topology != top {
				continue
			}
			origins := map[string]bool{}
			for _, ip := range s.CommunityOrigins {
				origins[ip] = true
			}
			opposite := "NodeB"
			if top == "A-O-C-B" {
				opposite = "NodeA"
			}
			for _, h := range s.Hops {
				if h.Owner == opposite {
					ip, _, _ := net.SplitHostPort(h.Local)
					origins[ip] = true
				}
			}
			both := origins[aip] && origins[bip]
			checks = append(checks, result(top+"_collusion_negative_control_exposes_both_origins", both))
			collusion = append(collusion, map[string]any{"topology": top, "both_origins_known": both, "strong_privacy_condition_satisfied": false, "evidence": "C socket remote plus opposite Node socket local; knowing a test flow is shared"})
			break
		}
	}
	ok := true
	for _, c := range checks {
		fmt.Fprintf(os.Stderr, "%s %s\n", c.Status, c.Name)
		ok = ok && c.Status == "PASS"
	}
	report := map[string]any{"prototype": "THROWAWAY Prompt48E protocol prototype, NOT product responses", "isolation": "one process, logical roles, Linux loopback identities; NOT separate namespaces", "checks": checks, "samples": samples, "medians": medians, "protocol_checks_pass": ok, "deployment_verdict": "INVALIDATED for strong collusion privacy and operator-egress-saving goals; DO NOT SHIP", "collusion_negative_controls": collusion, "operator_application_egress_saving_bytes": map[string]float64{"A-C-O-B": medians["A-O-B"]["operator_application_egress_bytes"] - medians["A-C-O-B"]["operator_application_egress_bytes"], "A-O-C-B": medians["A-O-B"]["operator_application_egress_bytes"] - medians["A-O-C-B"]["operator_application_egress_bytes"]}, "limitations": []string{"Community colluding with receiving Node, or operating as the opposite Node, can link A to B; strong collusion condition NOT met", "Operator intentionally sees grants and full hash; community sees adjacent origin and TLS timing/length", "TLS wire bytes count successful net.Conn writes outside all TLS, not IP/TCP headers or retransmits", "Loopback one-host throughput includes connect/handshake, payload generation and hash verification; no WAN/CPU isolation", "In-process Core mock has separate ephemeral Ed25519 admission and grant keys, distinct kind domains; no product API compatibility or production security claim", "Streaming capture scans all decrypted outer bytes with separate per-direction tails; any packed-IP collision is a conservative FAIL, no filtering; absence is not a cryptographic proof"}}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	must(enc.Encode(report))
	if !ok {
		os.Exit(1)
	}
}
