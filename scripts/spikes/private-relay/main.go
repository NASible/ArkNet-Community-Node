// Command relayspike is a THROWAWAY proof of concept for Prompt 48 (private
// relay design spike). It is not product code and is not part of the gate.
//
// Subcommands:
//
//	keygen DIR                          Ed25519 test key (tracker-grant format)
//	grant KEY LABEL HASH ACTION TTL     issue a tracker-format grant (v2, 58 bytes)
//	relay -verify PUB -cert-out FILE    the relay (TLS :7443, admin HTTP 127.0.0.1:7080)
//	socks -relay H:P -ca FILE -grants FILE      downloader-side connector (SOCKS5 :1080)
//	listen -relay H:P -ca FILE -grants FILE -target H:P -endpoint-out FILE
//	                                    seeder-side connector
//
// The relay authorizes with the same Core-signed, swarm-scoped grants the
// private tracker verifies: a seeder registers with a seed grant and receives
// an opaque virtual endpoint (100.64.0.0/10); a downloader connects to that
// endpoint with a download grant for the same info hash. The relay splices
// the two streams. Neither side is ever told the other's address.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	actionDownload = 0x01
	actionSeed     = 0x02
	headerLimit    = 16 << 10
	acceptTimeout  = 10 * time.Second
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		log.Fatal("usage: relayspike keygen|grant|relay|socks|listen ...")
	}
	switch os.Args[1] {
	case "keygen":
		keygen(os.Args[2])
	case "grant":
		fmt.Print(issue(os.Args[2], os.Args[3], os.Args[4], os.Args[5], os.Args[6]))
	case "relay":
		runRelay(os.Args[2:])
	case "socks":
		runSocks(os.Args[2:])
	case "listen":
		runListen(os.Args[2:])
	default:
		log.Fatal("unknown subcommand")
	}
}

// ---------------------------------------------------------------- grants

type grant struct {
	expires time.Time
	subject [16]byte
	hash    [32]byte
	actions byte
}

func keygen(dir string) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	check(err)
	check(os.WriteFile(filepath.Join(dir, "private.key"), []byte(base64.StdEncoding.EncodeToString(priv)), 0o600))
	check(os.WriteFile(filepath.Join(dir, "public.key"), []byte(base64.StdEncoding.EncodeToString(pub)), 0o644))
}

func subjectFor(label string) [16]byte {
	sum := sha256.Sum256([]byte("spike-subject\x00" + label))
	var s [16]byte
	copy(s[:], sum[:16])
	return s
}

func issue(keyFile, label, hashHex, action, ttl string) string {
	raw, err := os.ReadFile(keyFile)
	check(err)
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	check(err)
	hash, err := hex.DecodeString(hashHex)
	if err != nil || len(hash) != 32 {
		log.Fatal("bad hash")
	}
	seconds, err := time.ParseDuration(ttl + "s")
	check(err)
	payload := make([]byte, 58)
	payload[0] = 2
	binary.BigEndian.PutUint64(payload[1:9], uint64(time.Now().Add(seconds).Unix()))
	subject := subjectFor(label)
	copy(payload[9:25], subject[:])
	copy(payload[25:57], hash)
	payload[57] = map[string]byte{"download": actionDownload, "seed": actionSeed}[action]
	sig := ed25519.Sign(ed25519.PrivateKey(key), payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func verify(token string, pub ed25519.PublicKey, need byte, now time.Time) (grant, error) {
	var g grant
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return g, errors.New("invalid")
	}
	enc := base64.RawURLEncoding.Strict()
	payload, err1 := enc.DecodeString(parts[0])
	sig, err2 := enc.DecodeString(parts[1])
	if err1 != nil || err2 != nil || len(payload) != 58 || payload[0] != 2 || !ed25519.Verify(pub, payload, sig) {
		return g, errors.New("invalid")
	}
	g.expires = time.Unix(int64(binary.BigEndian.Uint64(payload[1:9])), 0)
	copy(g.subject[:], payload[9:25])
	copy(g.hash[:], payload[25:57])
	g.actions = payload[57]
	if g.actions&need == 0 {
		return g, errors.New("action")
	}
	if !now.Before(g.expires) {
		return g, errors.New("expired")
	}
	return g, nil
}

// ----------------------------------------------------------------- relay

type hello struct {
	Op      string   `json:"op"`
	Grant   string   `json:"grant,omitempty"`
	Grants  []string `json:"grants,omitempty"`
	Target  string   `json:"target,omitempty"`
	Session string   `json:"session,omitempty"`
}

type stream struct {
	conn net.Conn
	r    *bufio.Reader
}

type listener struct {
	endpoint string
	g        grant
	ctrl     net.Conn
	mu       sync.Mutex // serializes writes to ctrl
}

type pending struct {
	l  *listener
	ch chan stream
}

type session struct {
	subjects [2][16]byte
	conns    [2]net.Conn
}

type relay struct {
	pub       ed25519.PublicKey
	mu        sync.Mutex
	listeners map[string]*listener
	pending   map[string]pending
	sessions  map[*session]struct{}
	revoked   map[[16]byte]time.Time
	counters  sync.Map // name -> *atomic.Int64
}

func (r *relay) count(name string, n int64) {
	v, _ := r.counters.LoadOrStore(name, new(atomic.Int64))
	v.(*atomic.Int64).Add(n)
}

func (r *relay) isRevoked(s [16]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.revoked[s]
	return ok && time.Now().Before(until)
}

func runRelay(args []string) {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	verifyKey := fs.String("verify", "", "public key file")
	certOut := fs.String("cert-out", "", "write the relay's self-signed certificate here")
	addr := fs.String("listen", ":7443", "relay TLS address")
	admin := fs.String("admin", "127.0.0.1:7080", "admin HTTP address")
	check(fs.Parse(args))
	raw, err := os.ReadFile(*verifyKey)
	check(err)
	pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	check(err)
	r := &relay{pub: pub, listeners: map[string]*listener{}, pending: map[string]pending{},
		sessions: map[*session]struct{}{}, revoked: map[[16]byte]time.Time{}}
	cert := selfSigned(*certOut)
	ln, err := tls.Listen("tcp", *addr, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	check(err)
	go r.adminHTTP(*admin)
	log.Print("relay ready") // no addresses are ever logged
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go r.handle(conn)
	}
}

func selfSigned(out string) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relay"}, DNSNames: []string{"relay"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	check(err)
	check(os.WriteFile(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func writeLine(w io.Writer, v any) error {
	b, _ := json.Marshal(v)
	_, err := w.Write(append(b, '\n'))
	return err
}

func readLine(r *bufio.Reader, v any) error {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

func (r *relay) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReaderSize(conn, headerLimit)
	var h hello
	if err := readLine(br, &h); err != nil {
		r.count("refused_bad_header", 1)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch h.Op {
	case "listen":
		r.serveListen(conn, br, h)
	case "connect":
		r.serveConnect(conn, br, h)
	case "accept":
		r.serveAccept(conn, br, h)
	default:
		conn.Close()
	}
}

func randomEndpoint() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	ip := net.IPv4(100, 64|(b[0]&0x3f), b[1], b[2])
	port := 1024 + int(binary.BigEndian.Uint16(b[3:5]))%(65535-1024)
	return net.JoinHostPort(ip.String(), fmt.Sprint(port))
}

func (r *relay) serveListen(conn net.Conn, br *bufio.Reader, h hello) {
	g, err := verify(h.Grant, r.pub, actionSeed, time.Now())
	if err != nil || r.isRevoked(g.subject) {
		r.count("refused_listen", 1)
		_ = writeLine(conn, map[string]string{"error": "unauthorized"})
		conn.Close()
		return
	}
	l := &listener{endpoint: randomEndpoint(), g: g, ctrl: conn}
	r.mu.Lock()
	r.listeners[l.endpoint] = l
	r.mu.Unlock()
	r.count("listeners_total", 1)
	l.mu.Lock()
	_ = writeLine(conn, map[string]string{"endpoint": l.endpoint})
	l.mu.Unlock()
	// The registration ends with its grant (a product relay would accept renewals).
	timer := time.AfterFunc(time.Until(g.expires), func() { conn.Close() })
	defer timer.Stop()
	_, _ = io.Copy(io.Discard, br) // until the connector or the relay closes it
	r.mu.Lock()
	delete(r.listeners, l.endpoint)
	r.mu.Unlock()
	conn.Close()
}

func (r *relay) serveConnect(conn net.Conn, br *bufio.Reader, h hello) {
	r.mu.Lock()
	l := r.listeners[h.Target]
	r.mu.Unlock()
	if l == nil {
		r.count("refused_no_route", 1)
		_ = writeLine(conn, map[string]string{"error": "no_route"})
		conn.Close()
		return
	}
	var g grant
	ok := false
	for _, token := range h.Grants {
		if candidate, err := verify(token, r.pub, actionDownload, time.Now()); err == nil && candidate.hash == l.g.hash {
			g, ok = candidate, true
			break
		}
	}
	if !ok || r.isRevoked(g.subject) || r.isRevoked(l.g.subject) {
		r.count("refused_connect", 1)
		_ = writeLine(conn, map[string]string{"error": "unauthorized"})
		conn.Close()
		return
	}
	id := randomHex(16)
	ch := make(chan stream, 1)
	r.mu.Lock()
	r.pending[id] = pending{l: l, ch: ch}
	r.mu.Unlock()
	l.mu.Lock()
	err := writeLine(l.ctrl, map[string]string{"session": id})
	l.mu.Unlock()
	var seeder stream
	select {
	case seeder = <-ch:
	case <-time.After(acceptTimeout):
		err = errors.New("timeout")
	}
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
	if err != nil {
		r.count("refused_accept_timeout", 1)
		_ = writeLine(conn, map[string]string{"error": "unavailable"})
		conn.Close()
		return
	}
	if writeLine(conn, map[string]bool{"ok": true}) != nil {
		conn.Close()
		seeder.conn.Close()
		return
	}
	s := &session{subjects: [2][16]byte{g.subject, l.g.subject}, conns: [2]net.Conn{conn, seeder.conn}}
	r.mu.Lock()
	r.sessions[s] = struct{}{}
	r.mu.Unlock()
	r.count("sessions_total", 1)
	// A session never outlives either side's grant.
	deadline := g.expires
	if l.g.expires.Before(deadline) {
		deadline = l.g.expires
	}
	timer := time.AfterFunc(time.Until(deadline), func() { r.count("sessions_expired", 1); conn.Close(); seeder.conn.Close() })
	r.splice(stream{conn, br}, seeder)
	timer.Stop()
	r.mu.Lock()
	delete(r.sessions, s)
	r.mu.Unlock()
}

func (r *relay) serveAccept(conn net.Conn, br *bufio.Reader, h hello) {
	r.mu.Lock()
	p, ok := r.pending[h.Session]
	r.mu.Unlock()
	g, err := verify(h.Grant, r.pub, actionSeed, time.Now())
	if !ok || err != nil || g.subject != p.l.g.subject || g.hash != p.l.g.hash {
		r.count("refused_accept", 1)
		conn.Close()
		return
	}
	p.ch <- stream{conn, br}
}

func (r *relay) splice(down, seed stream) {
	var wg sync.WaitGroup
	pipe := func(dst net.Conn, src *bufio.Reader, counter string) {
		defer wg.Done()
		n, _ := io.CopyBuffer(dst, src, make([]byte, 256<<10))
		r.count(counter, n)
		down.conn.Close()
		seed.conn.Close()
	}
	wg.Add(2)
	go pipe(seed.conn, down.r, "bytes_to_seeder")
	go pipe(down.conn, seed.r, "bytes_to_downloader")
	wg.Wait()
}

func (r *relay) revoke(subject [16]byte) int {
	r.mu.Lock()
	r.revoked[subject] = time.Now().Add(10 * time.Minute) // longer than any grant
	var closing []net.Conn
	for endpoint, l := range r.listeners {
		if l.g.subject == subject {
			closing = append(closing, l.ctrl)
			delete(r.listeners, endpoint)
		}
	}
	for s := range r.sessions {
		if s.subjects[0] == subject || s.subjects[1] == subject {
			closing = append(closing, s.conns[0], s.conns[1])
		}
	}
	r.mu.Unlock()
	for _, c := range closing {
		c.Close()
	}
	return len(closing)
}

func (r *relay) adminHTTP(addr string) {
	mux := http.NewServeMux()
	// Stands in for the tracker, which would advertise relay endpoints instead of origins.
	mux.HandleFunc("GET /endpoints", func(w http.ResponseWriter, req *http.Request) {
		out := []string{}
		r.mu.Lock()
		for endpoint, l := range r.listeners {
			if hex.EncodeToString(l.g.hash[:]) == req.URL.Query().Get("hash") {
				out = append(out, endpoint)
			}
		}
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /revoke", func(w http.ResponseWriter, req *http.Request) {
		closed := r.revoke(subjectFor(req.URL.Query().Get("label")))
		_ = json.NewEncoder(w).Encode(map[string]any{"closed": closed, "at": float64(time.Now().UnixNano()) / 1e9})
	})
	mux.HandleFunc("GET /stats", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string]any{}
		r.counters.Range(func(k, v any) bool { out[k.(string)] = v.(*atomic.Int64).Load(); return true })
		var ru syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
		out["cpu_seconds"] = float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
		r.mu.Lock()
		out["sessions_active"], out["listeners_active"] = len(r.sessions), len(r.listeners)
		r.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	})
	check(http.ListenAndServe(addr, mux))
}

// ------------------------------------------------------------ connectors

type connectorFlags struct {
	relay, ca, grants, capture string
	captureLimit               int64
	tls                        *tls.Config
	captures                   atomic.Int64
}

func (c *connectorFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.relay, "relay", "relay:7443", "relay address")
	fs.StringVar(&c.ca, "ca", "", "relay certificate (pinned)")
	fs.StringVar(&c.grants, "grants", "", "file with one grant per line, re-read for every connection")
	fs.StringVar(&c.capture, "capture", "", "record bytes received from the relay under this prefix")
	fs.Int64Var(&c.captureLimit, "capture-limit", 4<<20, "bytes recorded per connection")
}

func (c *connectorFlags) init() {
	pemBytes, err := os.ReadFile(c.ca)
	check(err)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	c.tls = &tls.Config{RootCAs: pool, ServerName: "relay", MinVersion: tls.VersionTLS13}
}

func (c *connectorFlags) readGrants() []string {
	raw, err := os.ReadFile(c.grants)
	if err != nil {
		return nil
	}
	return strings.Fields(string(raw))
}

func (c *connectorFlags) dial() (*tls.Conn, error) {
	return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", c.relay, c.tls)
}

// recorder copies the first captureLimit bytes read from the relay to a file.
type recorder struct {
	r    io.Reader
	f    *os.File
	left int64
}

func (rec *recorder) Read(p []byte) (int, error) {
	n, err := rec.r.Read(p)
	if rec.f != nil && rec.left > 0 && n > 0 {
		k := int64(n)
		if k > rec.left {
			k = rec.left
		}
		_, _ = rec.f.Write(p[:k])
		rec.left -= k
	}
	return n, err
}

func (c *connectorFlags) recording(r io.Reader) (io.Reader, func()) {
	if c.capture == "" {
		return r, func() {}
	}
	f, err := os.Create(fmt.Sprintf("%s-%d.bin", c.capture, c.captures.Add(1)))
	if err != nil {
		return r, func() {}
	}
	return &recorder{r: r, f: f, left: c.captureLimit}, func() { f.Close() }
}

func pipe(local net.Conn, remote net.Conn, fromRemote io.Reader) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.CopyBuffer(remote, local, make([]byte, 256<<10)); done <- struct{}{} }()
	go func() { _, _ = io.CopyBuffer(local, fromRemote, make([]byte, 256<<10)); done <- struct{}{} }()
	<-done
	local.Close()
	remote.Close()
	<-done
}

var shared100 = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func runSocks(args []string) {
	var c connectorFlags
	fs := flag.NewFlagSet("socks", flag.ExitOnError)
	c.register(fs)
	listen := fs.String("listen", ":1080", "SOCKS5 address (private network only)")
	check(fs.Parse(args))
	c.init()
	ln, err := net.Listen("tcp", *listen)
	check(err)
	log.Print("socks connector ready")
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go c.socks(conn)
	}
}

// socks serves one SOCKS5 CONNECT. Only relay virtual endpoints are allowed:
// the connector never dials a peer directly, so relay failure fails closed.
func (c *connectorFlags) socks(conn net.Conn) {
	defer func() { recover() }()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != 5 {
		conn.Close()
		return
	}
	methods := make([]byte, head[1])
	_, _ = io.ReadFull(conn, methods)
	_, _ = conn.Write([]byte{5, 0})
	req := make([]byte, 10)
	if _, err := io.ReadFull(conn, req); err != nil || req[1] != 1 || req[3] != 1 {
		reply(conn, 7)
		return
	}
	ip := net.IP(req[4:8])
	port := binary.BigEndian.Uint16(req[8:10])
	if !shared100.Contains(ip) {
		reply(conn, 2) // not allowed: never a direct connection
		return
	}
	relayConn, err := c.dial()
	if err != nil {
		reply(conn, 1)
		return
	}
	target := net.JoinHostPort(ip.String(), fmt.Sprint(port))
	if writeLine(relayConn, hello{Op: "connect", Target: target, Grants: c.readGrants()}) != nil {
		relayConn.Close()
		reply(conn, 1)
		return
	}
	fromRelay, closeRec := c.recording(relayConn)
	defer closeRec()
	br := bufio.NewReader(fromRelay)
	var answer struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = relayConn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if readLine(br, &answer) != nil || !answer.OK {
		relayConn.Close()
		reply(conn, 5)
		return
	}
	_ = relayConn.SetReadDeadline(time.Time{})
	reply(conn, 0)
	_ = conn.SetDeadline(time.Time{})
	pipe(conn, relayConn, br)
}

func reply(conn net.Conn, code byte) {
	_, _ = conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	if code != 0 {
		conn.Close()
	}
}

func runListen(args []string) {
	var c connectorFlags
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	c.register(fs)
	target := fs.String("target", "", "local libtorrent listen address")
	endpointOut := fs.String("endpoint-out", "", "write the relay endpoint here")
	check(fs.Parse(args))
	c.init()
	grants := c.readGrants()
	if len(grants) == 0 {
		log.Fatal("no seed grant")
	}
	ctrl, err := c.dial()
	check(err)
	check(writeLine(ctrl, hello{Op: "listen", Grant: grants[0]}))
	fromRelay, closeRec := c.recording(ctrl)
	defer closeRec()
	br := bufio.NewReader(fromRelay)
	var registered struct {
		Endpoint string `json:"endpoint"`
		Error    string `json:"error"`
	}
	check(readLine(br, &registered))
	if registered.Endpoint == "" {
		log.Fatal("relay refused registration")
	}
	check(os.WriteFile(*endpointOut, []byte(registered.Endpoint), 0o644))
	log.Print("listen connector registered")
	for {
		var msg struct {
			Session string `json:"session"`
		}
		if err := readLine(br, &msg); err != nil {
			log.Fatal("relay control connection closed") // fail closed: no fallback listener
		}
		go func(id string) {
			relayConn, err := c.dial()
			if err != nil {
				return
			}
			if writeLine(relayConn, hello{Op: "accept", Session: id, Grant: grants[0]}) != nil {
				relayConn.Close()
				return
			}
			local, err := net.DialTimeout("tcp", *target, 5*time.Second)
			if err != nil {
				relayConn.Close()
				return
			}
			fromRelay, closeRec := c.recording(relayConn)
			defer closeRec()
			pipe(local, relayConn, fromRelay)
		}(msg.Session)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
