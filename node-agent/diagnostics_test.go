package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"arknet/node-agent/internal/application"
	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

type stubJellyfin struct {
	application.Adapter
	healthy bool
}

func (s stubJellyfin) Healthy(context.Context) bool { return s.healthy }

type downRepository struct{ testRepository }

func (downRepository) Ping(context.Context) error { return errors.New("connection refused") }
func (downRepository) Snapshot(context.Context) (localstate.Snapshot, error) {
	return localstate.Snapshot{}, errors.New("connection refused")
}

type listTransfers struct {
	transfer.Service
	list []transfer.Status
	err  error
}

func (l listTransfers) List(context.Context) ([]transfer.Status, error) { return l.list, l.err }

func diagnose(t *testing.T, repo localstate.Repository, opts ...handlerOption) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	newHandler(repo, opts...).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics", nil))
	if w.Code != 200 {
		t.Fatalf("diagnostics: %d %s", w.Code, w.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func field(body map[string]any, path ...string) any {
	var v any = body
	for _, p := range path {
		v = v.(map[string]any)[p]
	}
	return v
}

func fixedProbes(core, tracker string, coreHTTP int) diagnosticsOpt {
	return diagnosticsOpt{
		coreURL: "https://core.arknet.example", trackerOrigin: "https://tracker.arknet.example", mediaDir: "/srv/arknet/media",
		probeHTTP: func(_ context.Context, u string) (string, int) {
			if strings.HasPrefix(u, "https://core.arknet.example") {
				return core, coreHTTP
			}
			return tracker, 0
		},
		probeTailscale: func(context.Context) bool { return false },
		statfs: func(path string) (uint64, uint64, error) {
			if path != "/srv/arknet/media" {
				return 0, 0, errors.New("unexpected path " + path)
			}
			return 500 << 30, 120 << 30, nil
		},
	}
}

func TestDiagnosticsReportsEveryCheckWhenHealthy(t *testing.T) {
	selections := &selectionRepository{objects: []mediaselection.Object{
		{ID: "media_" + strings.Repeat("a", 64), Size: 3 << 30, LocalState: mediaselection.Available},
		{ID: "media_" + strings.Repeat("b", 64), Size: 1 << 30, LocalState: mediaselection.Downloading},
		{ID: "media_" + strings.Repeat("c", 64), Size: 9 << 30, LocalState: mediaselection.NotPresent},
	}}
	transfers := listTransfers{list: []transfer.Status{{State: transfer.Downloading}, {State: transfer.Paused}, {State: transfer.Completed},
		{State: transfer.VerificationFailed}, {State: transfer.Downloading, ErrorCode: "tracker_unreachable"}}}
	body := diagnose(t, testRepository{}, fixedProbes(probeOK, probeOK, 200), applicationOpt{stubJellyfin{healthy: true}},
		mediaOpt{repository: selections, allocation: 10 << 30}, transferOpt{service: transfers})
	want := map[string]any{"agent": "healthy", "database": "healthy", "jellyfin": "healthy", "tailscale": "not_detected"}
	for check, status := range want {
		if field(body, check, "status") != status {
			t.Fatalf("%s = %v", check, field(body, check, "status"))
		}
	}
	if field(body, "core", "status") != "ok" || field(body, "core", "host") != "core.arknet.example" || field(body, "core", "node_state") != "PROVISIONING" {
		t.Fatalf("core = %v", body["core"])
	}
	if field(body, "tracker", "status") != "ok" || field(body, "tracker", "host") != "tracker.arknet.example" {
		t.Fatalf("tracker = %v", body["tracker"])
	}
	if field(body, "storage", "kind") != "media" || field(body, "storage", "total_bytes") != float64(500<<30) || field(body, "storage", "free_bytes") != float64(120<<30) {
		t.Fatalf("storage = %v", body["storage"])
	}
	if field(body, "media", "reserved_bytes") != float64(4<<30) || field(body, "media", "allocation_bytes") != float64(10<<30) || field(body, "media", "selections") != float64(2) {
		t.Fatalf("media = %v", body["media"])
	}
	tr := body["transfers"].(map[string]any)
	if tr["active"] != float64(1) || tr["paused"] != float64(1) || tr["failed"] != float64(2) || tr["completed"] != float64(1) {
		t.Fatalf("transfers = %v", tr)
	}
	if field(body, "version", "agent") != version {
		t.Fatalf("version = %v", body["version"])
	}
	if strings.Contains(mustJSON(body), "token") || strings.Contains(mustJSON(body), "PGPASSWORD") {
		t.Fatal("diagnostics leaked configuration")
	}
}

func TestDiagnosticsKeepWorkingWhenDependenciesFail(t *testing.T) {
	body := diagnose(t, downRepository{}, fixedProbes(probeDNS, probeTimeout, 0), applicationOpt{stubJellyfin{healthy: false}},
		mediaOpt{repository: &failingSelections{}}, transferOpt{service: listTransfers{err: errors.New("down")}})
	for check, status := range map[string]string{"database": "unhealthy", "jellyfin": "unhealthy", "media": "unavailable", "transfers": "unavailable"} {
		if field(body, check, "status") != status {
			t.Fatalf("%s = %v", check, field(body, check, "status"))
		}
	}
	if field(body, "core", "status") != "dns" || field(body, "core", "node_state") != "UNKNOWN" || field(body, "tracker", "status") != "timeout" {
		t.Fatalf("core/tracker = %v %v", body["core"], body["tracker"])
	}
}

func TestDiagnosticsNotConfiguredParts(t *testing.T) {
	body := diagnose(t, testRepository{}, diagnosticsOpt{probeTailscale: func(context.Context) bool { return true },
		statfs: func(path string) (uint64, uint64, error) { return 100, 50, nil }})
	for check, status := range map[string]string{"core": "not_configured", "tracker": "not_configured", "jellyfin": "not_configured",
		"transfers": "not_configured", "tailscale": "detected"} {
		if field(body, check, "status") != status {
			t.Fatalf("%s = %v", check, field(body, check, "status"))
		}
	}
	if field(body, "storage", "kind") != "docker" || field(body, "core", "connectivity") != "NOT_CONFIGURED" {
		t.Fatalf("storage/core = %v %v", body["storage"], body["core"])
	}
}

func TestDiagnosticsAreBoundedWhenAProbeHangs(t *testing.T) {
	opt := fixedProbes(probeOK, probeOK, 200)
	opt.probeHTTP = func(ctx context.Context, _ string) (string, int) {
		<-ctx.Done()
		return classifyNetworkError(ctx.Err()), 0
	}
	started := time.Now()
	body := diagnose(t, testRepository{}, opt)
	if elapsed := time.Since(started); elapsed > diagnosticCheckTimeout+time.Second {
		t.Fatalf("diagnostics took %s", elapsed)
	}
	if field(body, "core", "status") != "timeout" || field(body, "database", "status") != "healthy" {
		t.Fatalf("hung probe: %v %v", body["core"], body["database"])
	}
}

func TestDiagnosticsTrackerAnyHTTPAnswerIsReachable(t *testing.T) {
	opt := fixedProbes(probeOK, probeServerError, 200)
	opt.probeHTTP = func(_ context.Context, u string) (string, int) {
		if strings.Contains(u, "tracker") {
			return probeServerError, 503
		}
		return probeServerError, 503
	}
	body := diagnose(t, testRepository{}, opt)
	if field(body, "tracker", "status") != "ok" || field(body, "core", "status") != "server_error" || field(body, "core", "http_status") != float64(503) {
		t.Fatalf("tracker/core = %v %v", body["tracker"], body["core"])
	}
}

func TestProbeURLClassifiesRealFailures(t *testing.T) {
	ctx := context.Background()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("probe sent credentials")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ok.Close()
	if status, code := probeURL(ctx, ok.URL+"/"); status != probeOK || code != 404 {
		t.Fatalf("ok: %s %d", status, code)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer broken.Close()
	if status, code := probeURL(ctx, broken.URL+"/health"); status != probeServerError || code != 503 {
		t.Fatalf("5xx: %s %d", status, code)
	}
	// Refused: a port that was just closed.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	if status, _ := probeURL(ctx, "http://"+addr+"/"); status != probeRefused {
		t.Fatalf("refused: %s", status)
	}
	// TLS: self-signed certificate is not trusted.
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	tlsServer.Config.ErrorLog = nil
	if status, _ := probeURL(ctx, tlsServer.URL+"/"); status != probeTLS {
		t.Fatalf("tls: %s", status)
	}
	// DNS: reserved .invalid TLD never resolves.
	if status, _ := probeURL(ctx, "https://core.arknet.invalid/health"); status != probeDNS {
		t.Fatalf("dns: %s", status)
	}
	// Timeout: a listener that never answers.
	hang, _ := net.Listen("tcp", "127.0.0.1:0")
	defer hang.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := hang.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			defer c.Close()
		}
	}()
	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if status, _ := probeURL(short, "http://"+hang.Addr().String()+"/"); status != probeTimeout {
		t.Fatalf("timeout: %s", status)
	}
	_ = tls.VersionTLS13
}

func TestDiagnosticsRejectsQuery(t *testing.T) {
	w := httptest.NewRecorder()
	newHandler(testRepository{}, diagnosticsOpt{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics?verbose=1", nil))
	if w.Code != 400 {
		t.Fatalf("query: %d", w.Code)
	}
}

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}

type failingSelections struct{ selectionRepository }

func (*failingSelections) ListMediaSelections(context.Context) ([]mediaselection.Object, error) {
	return nil, errors.New("db down")
}

var _ = catalog.Active
