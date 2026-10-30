package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/heartbeat"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

// Diagnostics reports machine-readable facts and failure categories; the UI
// turns them into operator explanations. Every check is bounded so one hung
// dependency cannot stall the page, and nothing here changes state.
const (
	diagnosticCheckTimeout = 4 * time.Second
	tailscaleProbeTimeout  = 1 * time.Second
	// Local database calls keep the Agent-wide 2 s bound.
	diagnosticDatabaseTimeout = 2 * time.Second
	// tailscaleResolver is Tailscale's on-host "quad100" resolver; it answers
	// only when the host runs Tailscale. Probing needs no socket or privilege.
	tailscaleResolver = "100.100.100.100:53"
)

// Probe outcome codes shared by Core and tracker checks.
const (
	probeOK             = "ok"
	probeDNS            = "dns"         // name does not resolve
	probeRefused        = "refused"     // host reachable, nothing listening
	probeTimeout        = "timeout"     // no answer (firewall, offline network)
	probeNoRoute        = "unreachable" // no network route
	probeTLS            = "tls"         // certificate or TLS handshake rejected
	probeServerError    = "server_error"
	probeOther          = "error"
	checkNotConfigured  = "not_configured"
	checkUnavailable    = "unavailable"
	checkHealthy        = "healthy"
	checkUnhealthy      = "unhealthy"
	tailscaleDetected   = "detected"
	tailscaleNotPresent = "not_detected"
)

type diagnosticsOpt struct {
	coreURL       string
	trackerOrigin string
	mediaDir      string
	// Test seams; nil uses the real implementations.
	probeHTTP      func(ctx context.Context, rawURL string) (string, int)
	probeTailscale func(ctx context.Context) bool
	statfs         func(path string) (total, free uint64, err error)
	now            func() time.Time
}

func (diagnosticsOpt) isOption() {}

type diagnosticCheck struct {
	Status string `json:"status"`
}

type coreDiagnostic struct {
	Status          string     `json:"status"` // probe code or not_configured
	Host            string     `json:"host,omitempty"`
	HTTPStatus      int        `json:"http_status,omitempty"`
	NodeState       string     `json:"node_state"`
	EnrollmentState string     `json:"enrollment_state"`
	Connectivity    string     `json:"connectivity"`
	LastContactAt   *time.Time `json:"last_contact_at"`
}

type trackerDiagnostic struct {
	Status string `json:"status"`
	Host   string `json:"host,omitempty"`
}

type storageDiagnostic struct {
	Status     string `json:"status"`
	Kind       string `json:"kind,omitempty"` // "media" volume or "docker" data filesystem
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

type mediaDiagnostic struct {
	Status          string `json:"status"`
	AllocationBytes int64  `json:"allocation_bytes"`
	ReservedBytes   int64  `json:"reserved_bytes"`
	Selections      int    `json:"selections"`
}

type transfersDiagnostic struct {
	Status    string `json:"status"`
	Active    int    `json:"active"`
	Paused    int    `json:"paused"`
	Failed    int    `json:"failed"`
	Completed int    `json:"completed"`
}

type versionDiagnostic struct {
	Agent   string          `json:"agent"`
	Release *releaseSummary `json:"release"`
}

type diagnosticsResponse struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Version     versionDiagnostic   `json:"version"`
	Agent       diagnosticCheck     `json:"agent"`
	Database    diagnosticCheck     `json:"database"`
	Jellyfin    diagnosticCheck     `json:"jellyfin"`
	Core        coreDiagnostic      `json:"core"`
	Tracker     trackerDiagnostic   `json:"tracker"`
	Tailscale   diagnosticCheck     `json:"tailscale"`
	Storage     storageDiagnostic   `json:"storage"`
	Media       mediaDiagnostic     `json:"media"`
	Transfers   transfersDiagnostic `json:"transfers"`
}

type diagnosticsDeps struct {
	opt        diagnosticsOpt
	repository localstate.Repository
	monitor    *heartbeat.Monitor
	jellyfin   application.Adapter
	media      mediaOpt
	transfers  transfer.Service
	release    *releaseSummary
}

func registerDiagnostics(mux *routeMux, d diagnosticsDeps) {
	mux.HandleFunc("GET /api/v1/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_request"})
			return
		}
		_ = json.NewEncoder(w).Encode(d.run(r.Context()))
	})
}

func (d diagnosticsDeps) run(parent context.Context) diagnosticsResponse {
	now := time.Now
	if d.opt.now != nil {
		now = d.opt.now
	}
	out := diagnosticsResponse{
		GeneratedAt: now().UTC(),
		Version:     versionDiagnostic{Agent: version, Release: d.release},
		Agent:       diagnosticCheck{Status: checkHealthy}, // answering this request
	}
	var wg sync.WaitGroup
	run := func(f func(ctx context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, diagnosticCheckTimeout)
			defer cancel()
			f(ctx)
		}()
	}
	run(func(ctx context.Context) { out.Database = d.database(ctx) })
	run(func(ctx context.Context) { out.Core = d.core(ctx) })
	run(func(ctx context.Context) { out.Jellyfin = d.jellyfinCheck(ctx) })
	run(func(ctx context.Context) { out.Tracker = d.tracker(ctx) })
	run(func(ctx context.Context) { out.Tailscale = d.tailscale(ctx) })
	run(func(ctx context.Context) { out.Storage = d.storage() })
	run(func(ctx context.Context) { out.Media = d.mediaUsage(ctx) })
	run(func(ctx context.Context) { out.Transfers = d.transferCounts(ctx) })
	wg.Wait()
	return out
}

func (d diagnosticsDeps) database(ctx context.Context) diagnosticCheck {
	ctx, cancel := context.WithTimeout(ctx, diagnosticDatabaseTimeout)
	defer cancel()
	if d.repository == nil || d.repository.Ping(ctx) != nil {
		return diagnosticCheck{Status: checkUnhealthy}
	}
	return diagnosticCheck{Status: checkHealthy}
}

func (d diagnosticsDeps) core(ctx context.Context) coreDiagnostic {
	result := coreDiagnostic{Status: checkNotConfigured, NodeState: "UNKNOWN", EnrollmentState: "UNKNOWN", Connectivity: "NOT_CONFIGURED"}
	if d.repository != nil {
		dbCtx, cancel := context.WithTimeout(ctx, diagnosticDatabaseTimeout)
		snapshot, err := d.repository.Snapshot(dbCtx)
		cancel()
		if err == nil {
			result.NodeState, result.EnrollmentState = string(snapshot.State), snapshot.EnrollmentState
			if !snapshot.LastSuccessfulCoreContactAt.IsZero() {
				contact := snapshot.LastSuccessfulCoreContactAt.UTC()
				result.LastContactAt = &contact
			}
		}
	}
	if d.opt.coreURL == "" {
		return result
	}
	result.Connectivity = d.monitor.Connectivity()
	result.Host = hostOf(d.opt.coreURL)
	result.Status, result.HTTPStatus = d.probe(ctx, d.opt.coreURL+"/health")
	return result
}

func (d diagnosticsDeps) tracker(ctx context.Context) trackerDiagnostic {
	if d.opt.trackerOrigin == "" {
		return trackerDiagnostic{Status: checkNotConfigured}
	}
	status, code := d.probe(ctx, d.opt.trackerOrigin+"/")
	if status == probeServerError && code > 0 {
		status = probeOK // any HTTP answer proves the tracker is reachable; it requires swarm authorization
	}
	return trackerDiagnostic{Status: status, Host: hostOf(d.opt.trackerOrigin)}
}

func (d diagnosticsDeps) probe(ctx context.Context, rawURL string) (string, int) {
	if d.opt.probeHTTP != nil {
		return d.opt.probeHTTP(ctx, rawURL)
	}
	return probeURL(ctx, rawURL)
}

func (d diagnosticsDeps) jellyfinCheck(ctx context.Context) diagnosticCheck {
	if d.jellyfin == nil {
		return diagnosticCheck{Status: checkNotConfigured}
	}
	if d.jellyfin.Healthy(ctx) {
		return diagnosticCheck{Status: checkHealthy}
	}
	return diagnosticCheck{Status: checkUnhealthy}
}

func (d diagnosticsDeps) tailscale(ctx context.Context) diagnosticCheck {
	detected := false
	if d.opt.probeTailscale != nil {
		detected = d.opt.probeTailscale(ctx)
	} else {
		detected = probeQuad100(ctx)
	}
	if detected {
		return diagnosticCheck{Status: tailscaleDetected}
	}
	return diagnosticCheck{Status: tailscaleNotPresent}
}

func (d diagnosticsDeps) storage() storageDiagnostic {
	path, kind := "/", "docker"
	if d.opt.mediaDir != "" {
		path, kind = d.opt.mediaDir, "media"
	}
	statfs := d.opt.statfs
	if statfs == nil {
		statfs = filesystemUsage
	}
	total, free, err := statfs(path)
	if err != nil || total == 0 {
		return storageDiagnostic{Status: checkUnavailable, Kind: kind}
	}
	return storageDiagnostic{Status: checkHealthy, Kind: kind, TotalBytes: total, FreeBytes: free}
}

func (d diagnosticsDeps) mediaUsage(ctx context.Context) mediaDiagnostic {
	result := mediaDiagnostic{Status: checkUnavailable, AllocationBytes: d.media.allocation}
	if d.media.repository == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, diagnosticDatabaseTimeout)
	defer cancel()
	selections, err := d.media.repository.ListMediaSelections(ctx)
	if err != nil {
		return result
	}
	for _, s := range selections {
		if s.LocalState == mediaselection.NotPresent {
			continue
		}
		if s.Size < 0 || s.Size > math.MaxInt64-result.ReservedBytes {
			return mediaDiagnostic{Status: checkUnavailable, AllocationBytes: d.media.allocation}
		}
		result.ReservedBytes += s.Size
		result.Selections++
	}
	result.Status = checkHealthy
	return result
}

func (d diagnosticsDeps) transferCounts(ctx context.Context) transfersDiagnostic {
	if d.transfers == nil {
		return transfersDiagnostic{Status: checkNotConfigured}
	}
	list, err := d.transfers.List(ctx)
	if err != nil {
		return transfersDiagnostic{Status: checkUnavailable}
	}
	result := transfersDiagnostic{Status: checkHealthy}
	for _, t := range list {
		switch {
		case t.State == transfer.VerificationFailed || t.ErrorCode != "":
			result.Failed++
		case t.State == transfer.Downloading:
			result.Active++
		case t.State == transfer.Paused:
			result.Paused++
		case t.State == transfer.Completed:
			result.Completed++
		}
	}
	return result
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Host
}

var diagnosticClient = &http.Client{
	Timeout:       diagnosticCheckTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		Proxy:               nil,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 3 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	},
}

// probeURL issues one unauthenticated GET and classifies the outcome. No
// credentials or Node identity are sent.
func probeURL(ctx context.Context, rawURL string) (string, int) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return probeOther, 0
	}
	request.Header.Set("User-Agent", "arknet-agent-diagnostics")
	response, err := diagnosticClient.Do(request)
	if err != nil {
		return classifyNetworkError(err), 0
	}
	response.Body.Close()
	if response.StatusCode >= 500 {
		return probeServerError, response.StatusCode
	}
	return probeOK, response.StatusCode
}

func classifyNetworkError(err error) string {
	var dnsError *net.DNSError
	var certInvalid x509.CertificateInvalidError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var tlsRecord tls.RecordHeaderError
	var certVerify *tls.CertificateVerificationError
	var timeout interface{ Timeout() bool }
	switch {
	case errors.As(err, &dnsError):
		return probeDNS
	case errors.As(err, &certVerify), errors.As(err, &certInvalid), errors.As(err, &unknownAuthority),
		errors.As(err, &hostname), errors.As(err, &tlsRecord):
		return probeTLS
	case errors.Is(err, syscall.ECONNREFUSED):
		return probeRefused
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return probeNoRoute
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &timeout) && timeout.Timeout():
		return probeTimeout
	}
	return probeOther
}

// probeQuad100 sends one DNS query to Tailscale's on-host resolver.
func probeQuad100(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, tailscaleProbeTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", tailscaleResolver)
	if err != nil {
		return false
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	// Minimal DNS query: id 0xA4E7, RD set, one question "tailscale.com. A IN".
	query := []byte{0xa4, 0xe7, 0x01, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0,
		9, 't', 'a', 'i', 'l', 's', 'c', 'a', 'l', 'e', 3, 'c', 'o', 'm', 0, 0, 1, 0, 1}
	if _, err := conn.Write(query); err != nil {
		return false
	}
	answer := make([]byte, 512)
	n, err := conn.Read(answer)
	return err == nil && n >= 12 && answer[0] == 0xa4 && answer[1] == 0xe7 && answer[2]&0x80 != 0
}

func filesystemUsage(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	block := uint64(st.Bsize)
	return st.Blocks * block, st.Bavail * block, nil
}
