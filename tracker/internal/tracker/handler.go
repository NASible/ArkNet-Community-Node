package tracker

import (
	"crypto/ed25519"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"arknet.local/tracker/internal/grant"
)

const (
	MaxPeersPerSwarm      = 1024
	maxRequestTargetBytes = 8192
)

type clockFunc func() time.Time

type peer struct {
	id               [20]byte
	ip               net.IP
	port             uint16
	complete         bool
	seen             time.Time
	grantExpiresUnix int64
}

type handler struct {
	publicKey ed25519.PublicKey
	interval  time.Duration
	peerTTL   time.Duration
	now       clockFunc
	mu        sync.Mutex
	swarms    map[[32]byte]map[[16]byte]peer
}

func NewHandler(publicKey ed25519.PublicKey, interval, peerTTL time.Duration, now clockFunc) http.Handler {
	h := &handler{
		publicKey: append(ed25519.PublicKey(nil), publicKey...),
		interval:  interval,
		peerTTL:   peerTTL,
		now:       now,
		swarms:    make(map[[32]byte]map[[16]byte]peer),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /announce/{grant}", h.announce)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.RequestURI) > maxRequestTargetBytes {
			writeFailure(w, http.StatusRequestURITooLong, "request target too long")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (h *handler) announce(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	verified, err := grant.Verify(r.PathValue("grant"), h.publicKey, now)
	if err != nil {
		writeFailure(w, http.StatusForbidden, "invalid grant")
		return
	}
	request, err := parseAnnounce(r)
	if err != nil || string(verified.Hash[:20]) != string(request.infoHash[:]) {
		writeFailure(w, http.StatusBadRequest, "invalid announce")
		return
	}
	requiredAction := grant.ActionSeed
	if request.left > 0 {
		requiredAction = grant.ActionDownload
	}
	if verified.Actions&requiredAction == 0 {
		writeFailure(w, http.StatusForbidden, "grant action not authorized")
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	swarm := h.swarms[verified.Hash]
	if swarm == nil {
		swarm = make(map[[16]byte]peer)
		h.swarms[verified.Hash] = swarm
	}
	for subject, candidate := range swarm {
		if now.Unix() >= candidate.grantExpiresUnix || now.Sub(candidate.seen) >= h.peerTTL {
			delete(swarm, subject)
		}
	}
	for subject, candidate := range swarm {
		if subject != verified.Subject && candidate.id == request.peerID {
			writeFailure(w, http.StatusConflict, "peer id belongs to another grant subject")
			return
		}
	}
	if request.event == "stopped" {
		candidate, exists := swarm[verified.Subject]
		if !exists || candidate.id != request.peerID {
			writeFailure(w, http.StatusConflict, "peer is not owned by grant subject")
			return
		}
		delete(swarm, verified.Subject)
	} else {
		if _, exists := swarm[verified.Subject]; !exists && len(swarm) >= MaxPeersPerSwarm {
			writeFailure(w, http.StatusServiceUnavailable, "swarm capacity reached")
			return
		}
		swarm[verified.Subject] = peer{
			id: request.peerID, ip: request.ip, port: request.port,
			complete: request.left == 0, seen: now, grantExpiresUnix: verified.ExpiresUnix,
		}
	}

	complete, incomplete := 0, 0
	peers4 := make([]byte, 0, request.numWant*6)
	peers6 := make([]byte, 0, request.numWant*18)
	remaining := request.numWant
	for subject, candidate := range swarm {
		if candidate.complete {
			complete++
		} else {
			incomplete++
		}
		// Only seed-authorized complete peers are ever advertised. A Node that
		// is still receiving is never handed to anyone, so receiving cannot
		// imply serving its partial pieces (Peer Serving stays opt-in).
		if subject == verified.Subject || remaining == 0 || !candidate.complete {
			continue
		}
		if ip4 := candidate.ip.To4(); ip4 != nil {
			peers4 = append(peers4, ip4...)
			peers4 = append(peers4, byte(candidate.port>>8), byte(candidate.port))
		} else if ip6 := candidate.ip.To16(); ip6 != nil {
			peers6 = append(peers6, ip6...)
			peers6 = append(peers6, byte(candidate.port>>8), byte(candidate.port))
		} else {
			continue
		}
		remaining--
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "d8:completei%de10:incompletei%de8:intervali%de5:peers%d:", complete, incomplete, int64(h.interval/time.Second), len(peers4))
	_, _ = w.Write(peers4)
	fmt.Fprintf(w, "6:peers6%d:", len(peers6))
	_, _ = w.Write(peers6)
	_, _ = w.Write([]byte("e"))
}

type announceRequest struct {
	infoHash [20]byte
	peerID   [20]byte
	ip       net.IP
	port     uint16
	left     uint64
	event    string
	numWant  int
}

func parseAnnounce(r *http.Request) (announceRequest, error) {
	var result announceRequest
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return result, err
	}
	allowed := map[string]bool{
		"info_hash": true, "peer_id": true, "port": true, "uploaded": true,
		"downloaded": true, "left": true, "compact": true, "event": true, "numwant": true,
		"corrupt": true, "key": true, "no_peer_id": true, "supportcrypto": true, "redundant": true,
	}
	for name := range values {
		if !allowed[name] {
			return result, fmt.Errorf("unknown query parameter")
		}
	}
	one := func(name string) (string, error) {
		items, ok := values[name]
		if !ok || len(items) != 1 {
			return "", fmt.Errorf("invalid %s", name)
		}
		return items[0], nil
	}
	infoHash, err := one("info_hash")
	if err != nil || len(infoHash) != len(result.infoHash) {
		return result, fmt.Errorf("invalid info_hash")
	}
	copy(result.infoHash[:], infoHash)
	peerID, err := one("peer_id")
	if err != nil || len(peerID) != len(result.peerID) {
		return result, fmt.Errorf("invalid peer_id")
	}
	copy(result.peerID[:], peerID)
	port, err := parseUnsigned(values, "port", 16)
	if err != nil || port == 0 {
		return result, fmt.Errorf("invalid port")
	}
	result.port = uint16(port)
	for _, field := range []string{"uploaded", "downloaded"} {
		if _, err := parseUnsigned(values, field, 64); err != nil {
			return result, fmt.Errorf("invalid %s", field)
		}
	}
	result.left, err = parseUnsigned(values, "left", 64)
	if err != nil {
		return result, fmt.Errorf("invalid left")
	}
	compact, err := one("compact")
	if err != nil || compact != "1" {
		return result, fmt.Errorf("compact required")
	}
	result.event = ""
	if events, ok := values["event"]; ok {
		if len(events) != 1 || (events[0] != "started" && events[0] != "completed" && events[0] != "stopped") {
			return result, fmt.Errorf("invalid event")
		}
		result.event = events[0]
	}
	result.numWant = 50
	if numWant, ok := values["numwant"]; ok {
		if len(numWant) != 1 {
			return result, fmt.Errorf("invalid numwant")
		}
		parsed, err := parseDecimal(numWant[0], 31)
		if err != nil {
			return result, fmt.Errorf("invalid numwant")
		}
		result.numWant = int(parsed)
		if result.numWant > 50 {
			result.numWant = 50
		}
	}
	for _, name := range []string{"corrupt", "redundant"} {
		if items, ok := values[name]; ok {
			if len(items) != 1 {
				return result, fmt.Errorf("invalid %s", name)
			}
			if _, err := parseDecimal(items[0], 64); err != nil {
				return result, fmt.Errorf("invalid %s", name)
			}
		}
	}
	if keys, ok := values["key"]; ok {
		if len(keys) != 1 || !validPeerKey(keys[0]) {
			return result, fmt.Errorf("invalid key")
		}
	}
	for _, name := range []string{"no_peer_id", "supportcrypto"} {
		if items, ok := values[name]; ok {
			if len(items) != 1 || (items[0] != "0" && items[0] != "1") {
				return result, fmt.Errorf("invalid %s", name)
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return result, fmt.Errorf("invalid remote address")
	}
	result.ip = net.ParseIP(host)
	if result.ip == nil {
		return result, fmt.Errorf("invalid remote address")
	}
	return result, nil
}

func parseUnsigned(values url.Values, name string, bits int) (uint64, error) {
	items, ok := values[name]
	if !ok || len(items) != 1 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return parseDecimal(items[0], bits)
}

func parseDecimal(value string, bits int) (uint64, error) {
	if value == "" {
		return 0, strconv.ErrSyntax
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseUint(value, 10, bits)
}

func validPeerKey(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func writeFailure(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	fmt.Fprintf(w, "d14:failure reason%d:%se", len(reason), reason)
}
