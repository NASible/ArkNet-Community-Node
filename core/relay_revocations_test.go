package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"arknet/core/internal/relayrevocations"
)

const (
	revokedNode   = "node_" + "1111111111111111111111111111111111111111111111111111111111111111"
	unrelatedNode = "node_" + "2222222222222222222222222222222222222222222222222222222222222222"
)

type fakeRelayRevocations struct {
	mu       sync.Mutex
	events   map[string]time.Time
	acked    map[[2]string]bool
	attempts map[[2]string]relayrevocations.Task
	hashes   []string
	now      func() time.Time
}

func newFakeRelayRevocations(now func() time.Time, hashes []string, nodes ...string) *fakeRelayRevocations {
	f := &fakeRelayRevocations{events: map[string]time.Time{}, acked: map[[2]string]bool{},
		attempts: map[[2]string]relayrevocations.Task{}, hashes: hashes, now: now}
	for _, n := range nodes {
		f.events[n] = now()
	}
	return f
}

func (f *fakeRelayRevocations) PendingRelayRevocations(_ context.Context, relayIDs []string, limit int) ([]relayrevocations.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []relayrevocations.Task
	for node, decided := range f.events {
		for _, relay := range relayIDs {
			key := [2]string{node, relay}
			if f.acked[key] {
				continue
			}
			task := f.attempts[key]
			task.NodeID, task.RelayID, task.DecidedAt = node, relay, decided
			out = append(out, task)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeRelayRevocations) RecordRelayRevocationAttempt(_ context.Context, node, relay, failure string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := [2]string{node, relay}
	task := f.attempts[key]
	task.Attempts++
	task.LastError = failure
	task.LastAttemptAt = f.now()
	f.attempts[key] = task
	if failure == "" {
		f.acked[key] = true
	}
	return nil
}

func (f *fakeRelayRevocations) MediaInfoHashes(context.Context) ([]string, error) {
	return f.hashes, nil
}

func (f *fakeRelayRevocations) isAcked(node, relay string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acked[[2]string{node, relay}]
}

// fakeRelay verifies tokens exactly as relay/internal/revocation does and
// records the subjects it was told to revoke.
type fakeRelay struct {
	t        *testing.T
	id       string
	key      ed25519.PublicKey
	mu       sync.Mutex
	status   []int // answers to give, then 204
	subjects [][16]byte
	posts    int
	server   *httptest.Server
}

func newFakeRelay(t *testing.T, id string, key ed25519.PublicKey, status ...int) *fakeRelay {
	r := &fakeRelay{t: t, id: id, key: key, status: status}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	return r
}

func (r *fakeRelay) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts++
	if req.Method != http.MethodPost || req.URL.Path != "/api/relay/v1/revocations" {
		w.WriteHeader(404)
		return
	}
	if len(r.status) > 0 {
		code := r.status[0]
		r.status = r.status[1:]
		w.WriteHeader(code)
		return
	}
	body, _ := io.ReadAll(req.Body)
	encodedPayload, encodedSignature, _ := strings.Cut(string(body), ".")
	payload, _ := base64.RawURLEncoding.DecodeString(encodedPayload)
	signature, _ := base64.RawURLEncoding.DecodeString(encodedSignature)
	audience := sha256.Sum256([]byte(r.id))
	if len(payload) < 59 || !ed25519.Verify(r.key, append([]byte("ARKNET-RELAY-REVOCATION-V1\x00"), payload...), signature) ||
		!bytes.Equal(payload[25:57], audience[:]) {
		w.WriteHeader(401)
		return
	}
	issued := time.Unix(int64(binary.BigEndian.Uint64(payload[1:9])), 0)
	if d := time.Since(issued); d > time.Minute || d < -time.Minute {
		w.WriteHeader(401)
		return
	}
	count := int(binary.BigEndian.Uint16(payload[57:59]))
	for i := 0; i < count; i++ {
		var s [16]byte
		copy(s[:], payload[59+16*i:])
		r.subjects = append(r.subjects, s)
	}
	w.WriteHeader(204)
}

func (r *fakeRelay) received() ([][16]byte, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][16]byte(nil), r.subjects...), r.posts
}

type pushFixture struct {
	relays  *relaysConfig
	fake    []*fakeRelay
	repo    *fakeRelayRevocations
	tracker *trackerGrantIssuer
	clock   *clock
	logs    *bytes.Buffer
	pusher  *relayRevocationPusher
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func hashes(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%064x", i+1)
	}
	return out
}

func newPushFixture(t *testing.T, objectHashes []string, status [][]int, nodes ...string) *pushFixture {
	t.Helper()
	f := &pushFixture{relays: testRelays(t), clock: &clock{now: time.Now()}, logs: &bytes.Buffer{}}
	_, trackerKey, _ := ed25519.GenerateKey(nil)
	f.tracker = &trackerGrantIssuer{privateKey: trackerKey, now: f.clock.Now}
	for i := range f.relays.entries {
		entry := &f.relays.entries[i]
		var answers []int
		if i < len(status) {
			answers = status[i]
		}
		relay := newFakeRelay(t, entry.ID, entry.revocationKey.Public().(ed25519.PublicKey), answers...)
		entry.ControlURL, _ = url.Parse(relay.server.URL)
		f.fake = append(f.fake, relay)
	}
	f.repo = newFakeRelayRevocations(f.clock.Now, objectHashes, nodes...)
	f.pusher = newRelayRevocationPusher(f.relays, f.repo, f.tracker, f.clock.Now, slog.New(slog.NewJSONHandler(f.logs, nil)))
	return f
}

func (f *pushFixture) subjectsOf(t *testing.T, node string) map[[16]byte]bool {
	t.Helper()
	out := map[[16]byte]bool{}
	for _, h := range f.repo.hashes {
		raw, _ := hex.DecodeString(h)
		out[f.tracker.subject(node, raw)] = true
	}
	return out
}

func TestRevocationReachesEveryRelayWithTheNodesSubjects(t *testing.T) {
	f := newPushFixture(t, hashes(3), nil, revokedNode)
	if err := f.pusher.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := f.subjectsOf(t, revokedNode)
	for _, relay := range f.fake {
		got, _ := relay.received()
		if len(got) != 3 {
			t.Fatalf("%s got %d subjects", relay.id, len(got))
		}
		for _, s := range got {
			if !want[s] {
				t.Fatalf("%s got a subject that is not the revoked Node's", relay.id)
			}
		}
		if !f.repo.isAcked(revokedNode, relay.id) {
			t.Fatalf("%s delivery not recorded", relay.id)
		}
	}
	// The subjects are the ones in the Node's real tracker grants.
	grant, _, err := f.tracker.issueGrant(revokedNode, f.repo.hashes[0], trackerGrantActionDownload)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(strings.SplitN(grant, ".", 2)[0])
	var subject [16]byte
	copy(subject[:], payload[9:25])
	if !want[subject] {
		t.Fatal("revocation subjects differ from tracker grant subjects")
	}
}

func TestUnrelatedNodesAreUnaffected(t *testing.T) {
	f := newPushFixture(t, hashes(4), nil, revokedNode)
	if err := f.pusher.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	other := f.subjectsOf(t, unrelatedNode)
	for _, relay := range f.fake {
		got, _ := relay.received()
		for _, s := range got {
			if other[s] {
				t.Fatal("an unrelated Node's subject was revoked")
			}
		}
	}
	tasks, _ := f.repo.PendingRelayRevocations(context.Background(), []string{"relay-dc-1"}, 10)
	if len(tasks) != 0 {
		t.Fatalf("pending: %v", tasks)
	}
}

func TestLargeCatalogsAreSentInBoundedMessages(t *testing.T) {
	f := newPushFixture(t, hashes(maxRelayRevocationSubjects+5), nil, revokedNode)
	if err := f.pusher.once(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, posts := f.fake[0].received()
	if len(got) != maxRelayRevocationSubjects+5 || posts != 2 {
		t.Fatalf("subjects=%d posts=%d", len(got), posts)
	}
}

func TestRetryAfterARelayOutage(t *testing.T) {
	f := newPushFixture(t, hashes(2), [][]int{{503, 503}}, revokedNode)
	ctx := context.Background()
	_ = f.pusher.once(ctx)
	if f.repo.isAcked(revokedNode, "relay-dc-1") || !f.repo.isAcked(revokedNode, "relay-west-1") {
		t.Fatal("outage must not block the other relay or count as delivered")
	}
	task := f.repo.attempts[[2]string{revokedNode, "relay-dc-1"}]
	if task.Attempts != 1 || task.LastError != "status_503" {
		t.Fatalf("%+v", task)
	}
	_ = f.pusher.once(ctx) // inside the backoff: no new attempt
	if _, posts := f.fake[0].received(); posts != 1 {
		t.Fatalf("retried before the backoff: %d posts", posts)
	}
	f.clock.Advance(relayRevocationBackoff(1))
	_ = f.pusher.once(ctx)
	if f.repo.isAcked(revokedNode, "relay-dc-1") {
		t.Fatal("acknowledged on a second 503")
	}
	f.clock.Advance(relayRevocationBackoff(2))
	_ = f.pusher.once(ctx)
	if !f.repo.isAcked(revokedNode, "relay-dc-1") {
		t.Fatal("not delivered after the relay recovered")
	}
	if got, _ := f.fake[0].received(); len(got) != 2 {
		t.Fatalf("subjects after recovery: %d", len(got))
	}
}

func TestUnreachableRelayIsRetried(t *testing.T) {
	f := newPushFixture(t, hashes(1), nil, revokedNode)
	f.fake[0].server.Close()
	_ = f.pusher.once(context.Background())
	task := f.repo.attempts[[2]string{revokedNode, "relay-dc-1"}]
	if task.LastError != "unreachable" || f.repo.isAcked(revokedNode, "relay-dc-1") {
		t.Fatalf("%+v", task)
	}
}

func TestRelayRefusalIsAFailureAndReplayIsAnAcknowledgement(t *testing.T) {
	f := newPushFixture(t, hashes(1), [][]int{{401}, {409}}, revokedNode)
	_ = f.pusher.once(context.Background())
	if f.repo.isAcked(revokedNode, "relay-dc-1") || f.repo.attempts[[2]string{revokedNode, "relay-dc-1"}].LastError != "status_401" {
		t.Fatal("a refused revocation counted as delivered")
	}
	if !f.repo.isAcked(revokedNode, "relay-west-1") {
		t.Fatal("409 (already applied) must count as delivered")
	}
}

func TestEmptyCatalogNeedsNoPush(t *testing.T) {
	f := newPushFixture(t, nil, nil, revokedNode)
	_ = f.pusher.once(context.Background())
	if _, posts := f.fake[0].received(); posts != 0 || !f.repo.isAcked(revokedNode, "relay-dc-1") {
		t.Fatal("without objects there are no grants to cut, so delivery is complete")
	}
}

func TestBackoffIsBounded(t *testing.T) {
	if relayRevocationBackoff(0) != 0 || relayRevocationBackoff(1) != time.Second || relayRevocationBackoff(3) != 4*time.Second ||
		relayRevocationBackoff(40) != time.Minute {
		t.Fatal("backoff")
	}
}

func TestPushLogsCarryNoSubjectsOrNodeIDs(t *testing.T) {
	f := newPushFixture(t, hashes(2), [][]int{{503}}, revokedNode)
	_ = f.pusher.once(context.Background())
	logs := f.logs.String()
	if !strings.Contains(logs, "relay_revocation_failed") || !strings.Contains(logs, "relay_revocation_delivered") {
		t.Fatalf("missing events: %s", logs)
	}
	for s := range f.subjectsOf(t, revokedNode) {
		if strings.Contains(logs, hex.EncodeToString(s[:])) || strings.Contains(logs, base64.StdEncoding.EncodeToString(s[:])) {
			t.Fatal("log contains a subject")
		}
	}
	if strings.Contains(logs, revokedNode) || strings.Contains(logs, f.fake[0].server.URL) {
		t.Fatalf("log contains the Node ID or control URL: %s", logs)
	}
}

func TestAdministratorSeesUndeliveredRevocations(t *testing.T) {
	f := newPushFixture(t, hashes(1), [][]int{{503}}, revokedNode)
	_ = f.pusher.once(context.Background())
	h := withRelays(http.NotFoundHandler(), f.relays, nil, f.repo, allowAdmin(true), f.clock.Now)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/v1/relays", nil))
	var body struct {
		Relays      []map[string]any `json:"relays"`
		Undelivered []map[string]any `json:"undelivered_revocations"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Relays) != 2 || len(body.Undelivered) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	u := body.Undelivered[0]
	if u["node_id"] != revokedNode || u["relay_id"] != "relay-dc-1" || u["attempts"] != float64(1) || u["last_error"] != "status_503" {
		t.Fatalf("%v", u)
	}
	if strings.Contains(w.Body.String(), "subject") {
		t.Fatal("admin view exposes subjects")
	}
	denied := httptest.NewRecorder()
	withRelays(http.NotFoundHandler(), f.relays, nil, f.repo, allowAdmin(false), f.clock.Now).
		ServeHTTP(denied, httptest.NewRequest("GET", "/api/admin/v1/relays", nil))
	if denied.Code != 401 {
		t.Fatalf("unauthorized admin: %d", denied.Code)
	}
}
