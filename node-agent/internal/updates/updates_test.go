package updates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"arknet/node-agent/internal/release"
)

func manifestJSON(version string, channel release.Channel) []byte {
	digest := strings.Repeat("a", 64)
	comp := func(v, image string, bundled bool) map[string]any {
		return map[string]any{"version": v, "image": image, "bundled": bundled}
	}
	data, _ := json.Marshal(map[string]any{
		"schema": release.Schema, "version": version, "channel": channel, "published_at": "2026-10-26T00:00:00Z",
		"components": map[string]any{
			release.Agent: comp(version, "arknet-agent:"+version, true), release.UI: comp(version, "arknet-ui:"+version, true),
			release.Transfer: comp(version, "arknet-transfer:"+version, true), release.PostgreSQL: comp("18", "postgres:18-alpine", true),
			release.Jellyfin: comp("12.2", "jellyfin/jellyfin:12.2", true), release.Tailscale: comp("1.104.2", "tailscale/tailscale:v1.104.2", false),
		},
		"compose": map[string]any{"project": "arknet-node", "files": []any{map[string]any{"path": "compose.yaml", "sha256": digest}}},
	})
	return data
}

func current(t *testing.T, version string, channel release.Channel) release.Manifest {
	t.Helper()
	m, err := release.Parse(manifestJSON(version, channel))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

type publisher struct {
	mu        sync.Mutex
	index     func(base string) string
	manifests map[string][]byte
	requests  []*http.Request
	server    *httptest.Server
}

func newPublisher(t *testing.T) *publisher {
	p := &publisher{manifests: map[string][]byte{}}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.requests = append(p.requests, r.Clone(context.Background()))
		index, manifests := p.index, p.manifests
		p.mu.Unlock()
		if r.URL.Path == "/releases/index.json" {
			_, _ = w.Write([]byte(index(p.server.URL)))
			return
		}
		if data, ok := manifests[r.URL.Path]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *publisher) publish(path string, data []byte) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manifests[path] = data
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p *publisher) checker(t *testing.T) Checker {
	u, err := url.Parse(p.server.URL + "/releases/index.json")
	if err != nil {
		t.Fatal(err)
	}
	return Checker{IndexURL: u}
}

func indexWith(entries string) func(string) string {
	return func(string) string { return `{"schema":"` + IndexSchema + `","channels":{` + entries + `}}` }
}

func TestCheckDetectsNewerVerifiedReleaseOnOwnChannel(t *testing.T) {
	p := newPublisher(t)
	devSum := p.publish("/releases/0.0.2-dev.json", manifestJSON("0.0.2-dev", release.Development))
	stableSum := p.publish("/releases/0.1.0.json", manifestJSON("0.1.0", release.Stable))
	p.index = indexWith(`"development":{"version":"0.0.2-dev","manifest_url":"0.0.2-dev.json","manifest_sha256":"` + devSum + `"},` +
		`"stable":{"version":"0.1.0","manifest_url":"/releases/0.1.0.json","manifest_sha256":"` + stableSum + `"}`)
	status, err := p.checker(t).Check(context.Background(), current(t, "0.0.1-dev", release.Development))
	if err != nil || status.State != UpdateAvailable || status.Available.Version != "0.0.2-dev" || status.Available.Channel != release.Development {
		t.Fatalf("development: %+v %v", status, err)
	}
	// Stable Nodes see only the stable channel.
	status, err = p.checker(t).Check(context.Background(), current(t, "0.0.9", release.Stable))
	if err != nil || status.State != UpdateAvailable || status.Available.Version != "0.1.0" {
		t.Fatalf("stable: %+v %v", status, err)
	}
	for _, r := range p.requests {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.URL.RawQuery != "" || strings.Contains(r.Header.Get("User-Agent"), "node_") {
			t.Fatalf("discovery request disclosed identity: %v %v", r.URL, r.Header)
		}
	}
}

func TestCheckReportsUpToDateForSameOlderOrUnpublished(t *testing.T) {
	p := newPublisher(t)
	sum := p.publish("/releases/m.json", manifestJSON("0.0.1-dev", release.Development))
	for _, entries := range []string{
		`"development":{"version":"0.0.1-dev","manifest_url":"m.json","manifest_sha256":"` + sum + `"}`, // same
		`"development":{"version":"0.0.1-a","manifest_url":"m.json","manifest_sha256":"` + sum + `"}`,   // older
		``, // channel not published
	} {
		p.index = indexWith(entries)
		before := len(p.requests)
		status, err := p.checker(t).Check(context.Background(), current(t, "0.0.1-dev", release.Development))
		if err != nil || status.State != UpToDate || status.Available != nil || len(p.requests)-before != 1 {
			t.Fatalf("%s: %+v %v (requests %d)", entries, status, err, len(p.requests)-before)
		}
	}
}

func TestCheckRejectsUnverifiablePublications(t *testing.T) {
	cur := func(t *testing.T) release.Manifest { return current(t, "0.0.1-dev", release.Development) }
	good := manifestJSON("0.0.2-dev", release.Development)
	for name, setup := range map[string]func(p *publisher){
		"digest mismatch": func(p *publisher) {
			p.publish("/releases/m.json", good)
			p.index = indexWith(`"development":{"version":"0.0.2-dev","manifest_url":"m.json","manifest_sha256":"` + strings.Repeat("0", 64) + `"}`)
		},
		"manifest version differs from index": func(p *publisher) {
			sum := p.publish("/releases/m.json", good)
			p.index = indexWith(`"development":{"version":"0.0.3-dev","manifest_url":"m.json","manifest_sha256":"` + sum + `"}`)
		},
		"manifest on other channel": func(p *publisher) {
			sum := p.publish("/releases/m.json", manifestJSON("0.1.0", release.Stable))
			p.index = indexWith(`"development":{"version":"0.1.0-x","manifest_url":"m.json","manifest_sha256":"` + sum + `"}`)
		},
		"invalid manifest": func(p *publisher) {
			sum := p.publish("/releases/m.json", []byte(`{"schema":"x"}`))
			p.index = indexWith(`"development":{"version":"0.0.2-dev","manifest_url":"m.json","manifest_sha256":"` + sum + `"}`)
		},
		"missing manifest": func(p *publisher) {
			p.index = indexWith(`"development":{"version":"0.0.2-dev","manifest_url":"gone.json","manifest_sha256":"` + strings.Repeat("0", 64) + `"}`)
		},
		"plain HTTP manifest elsewhere": func(p *publisher) {
			p.index = indexWith(`"development":{"version":"0.0.2-dev","manifest_url":"http://releases.example/m.json","manifest_sha256":"` + strings.Repeat("0", 64) + `"}`)
		},
		"stable version on development channel": func(p *publisher) {
			p.index = indexWith(`"development":{"version":"0.1.0","manifest_url":"m.json","manifest_sha256":"` + strings.Repeat("0", 64) + `"}`)
		},
		"unknown channel": func(p *publisher) {
			p.index = indexWith(`"beta":{"version":"0.1.0-b","manifest_url":"m.json","manifest_sha256":"` + strings.Repeat("0", 64) + `"}`)
		},
		"unknown field": func(p *publisher) {
			p.index = func(string) string { return `{"schema":"` + IndexSchema + `","channels":{},"x":1}` }
		},
		"wrong schema": func(p *publisher) { p.index = func(string) string { return `{"schema":"other","channels":{}}` } },
		"oversized index": func(p *publisher) {
			p.index = func(string) string { return strings.Repeat(" ", MaxIndexBytes+1) + "{}" }
		},
		"not JSON": func(p *publisher) { p.index = func(string) string { return "<html>" } },
	} {
		t.Run(name, func(t *testing.T) {
			p := newPublisher(t)
			setup(p)
			status, err := p.checker(t).Check(context.Background(), cur(t))
			if err == nil || status.State != CheckFailed || status.Available != nil {
				t.Fatalf("accepted: %+v %v", status, err)
			}
		})
	}
}

func TestCheckDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect followed")
	}))
	defer target.Close()
	source := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer source.Close()
	u, _ := url.Parse(source.URL + "/index.json")
	if status, err := (Checker{IndexURL: u}).Check(context.Background(), current(t, "0.0.1-dev", release.Development)); !errors.Is(err, ErrFetch) || status.State != CheckFailed {
		t.Fatalf("redirect: %+v %v", status, err)
	}
}

func TestValidateURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://releases.arknet.example/index.json": true, "http://127.0.0.1:8080/i.json": true, "http://localhost/i.json": true,
		"http://releases.example/i.json": false, "ftp://x/i": false, "https://u:p@x/i": false, "/relative": false, "https://x/i#f": false,
	} {
		u, _ := url.Parse(raw)
		if (ValidateURL(u, false) == nil) != want {
			t.Fatalf("%s: want %v", raw, want)
		}
	}
	u, _ := url.Parse("http://release-index:8080/i.json")
	if ValidateURL(u, true) != nil {
		t.Fatal("explicit HTTP opt-in rejected")
	}
}

func TestMonitorStatesCoalescingAndRetainedAvailability(t *testing.T) {
	if s := NewMonitor(nil, nil).Status(); s.State != NotConfigured {
		t.Fatalf("nil checker = %s", s.State)
	}
	p := newPublisher(t)
	checker := p.checker(t)
	if s, _ := NewMonitor(&checker, nil).Check(context.Background(), true); s.State != NoCurrentRelease || len(p.requests) != 0 {
		t.Fatalf("no current release = %s, requests %d", s.State, len(p.requests))
	}
	sum := p.publish("/releases/m.json", manifestJSON("0.0.2-dev", release.Development))
	p.index = indexWith(`"development":{"version":"0.0.2-dev","manifest_url":"m.json","manifest_sha256":"` + sum + `"}`)
	cur := current(t, "0.0.1-dev", release.Development)
	m := NewMonitor(&checker, &cur)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	if m.Status().State != NotChecked {
		t.Fatal("initial state")
	}
	if s, err := m.Check(context.Background(), true); err != nil || s.State != UpdateAvailable || !s.CheckedAt.Equal(now) {
		t.Fatalf("first check: %+v %v", s, err)
	}
	requests := len(p.requests)
	if s, _ := m.Check(context.Background(), true); s.State != UpdateAvailable || len(p.requests) != requests {
		t.Fatal("manual check within MinManualInterval refetched")
	}
	now = now.Add(MinManualInterval)
	p.index = func(string) string { return "broken" }
	s, err := m.Check(context.Background(), true)
	if err == nil || s.State != CheckFailed || s.Available == nil || s.Available.Version != "0.0.2-dev" {
		t.Fatalf("transient failure should keep verified availability: %+v %v", s, err)
	}
}

func TestMonitorRunChecksImmediatelyAndStops(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"schema":"` + IndexSchema + `","channels":{}}`))
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL + "/i.json")
	cur := current(t, "0.0.1-dev", release.Development)
	m := NewMonitor(&Checker{IndexURL: u}, &cur)
	ctx, cancel := context.WithCancel(context.Background())
	done := m.Run(ctx, time.Hour, nil)
	deadline := time.Now().Add(2 * time.Second)
	for m.Status().State != UpToDate && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if m.Status().State != UpToDate || hits.Load() != 1 {
		t.Fatalf("run: %s hits=%d", m.Status().State, hits.Load())
	}
}
