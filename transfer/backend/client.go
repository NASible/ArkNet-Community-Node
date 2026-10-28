// Package backend runs the libtorrent worker behind a strict local JSON-lines protocol.
package backend

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"arknet/transfer/engine"
)

type Config struct {
	Executable        string
	TrackerOrigin     string
	TrackerAllowHTTP  bool
	WebSeedOrigin     string
	WebSeedAllowHTTP  bool
	StagingDir        string
	StateDir          string
	ListenInterfaces  string
	DownloadRateLimit int64
}

type Client struct {
	mu            sync.Mutex
	cmd           *exec.Cmd
	in            io.WriteCloser
	out           *bufio.Reader
	next          int64
	closed        bool
	webSeedOrigin string
	trackerOrigin string
}

type policy struct {
	V2Only bool `json:"v2_only"`
	DHT    bool `json:"dht"`
	PEX    bool `json:"pex"`
	LSD    bool `json:"lsd"`
}
type ready struct {
	Ready   bool   `json:"ready"`
	Backend string `json:"backend"`
	Version string `json:"version"`
	Policy  policy `json:"policy"`
}
type response struct {
	ID        int64           `json:"id"`
	Status    *engine.Status  `json:"status,omitempty"`
	Transfers []engine.Status `json:"transfers,omitempty"`
	Seed      *workerSeed     `json:"seed,omitempty"`
	Seeds     []workerSeed    `json:"seeds,omitempty"`
	Error     string          `json:"error,omitempty"`
	OK        bool            `json:"ok,omitempty"`
}

// workerSeed is the worker's seed record. Byte counters stay inside the
// transfer service; the HTTP API exposes only object ID and state.
type workerSeed struct {
	ObjectID      string           `json:"object_id"`
	State         engine.SeedState `json:"state"`
	TotalBytes    int64            `json:"total_bytes"`
	UploadedBytes int64            `json:"uploaded_bytes"`
}

func Start(ctx context.Context, cfg Config) (*Client, error) {
	for _, dir := range []string{cfg.StagingDir, cfg.StateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, errors.New("worker unavailable")
		}
	}
	cmd := exec.Command(cfg.Executable)
	cmd.Env = append(os.Environ(),
		"ARKNET_TRANSFER_TRACKER_ORIGIN="+cfg.TrackerOrigin,
		"ARKNET_TRANSFER_TRACKER_ALLOW_HTTP="+strconv.FormatBool(cfg.TrackerAllowHTTP),
		"ARKNET_TRANSFER_WEB_SEED_ORIGIN="+cfg.WebSeedOrigin,
		"ARKNET_TRANSFER_WEB_SEED_ALLOW_HTTP="+strconv.FormatBool(cfg.WebSeedAllowHTTP),
		"ARKNET_TRANSFER_STAGING_DIR="+cfg.StagingDir,
		"ARKNET_TRANSFER_STATE_DIR="+cfg.StateDir,
		"ARKNET_TRANSFER_LISTEN_INTERFACES="+cfg.ListenInterfaces,
		"ARKNET_TRANSFER_DOWNLOAD_RATE_LIMIT="+strconv.FormatInt(cfg.DownloadRateLimit, 10),
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("worker unavailable")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("worker unavailable")
	}
	// Worker diagnostics are deliberately structured and exclude URLs, hashes,
	// object IDs, and bearer credentials.
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, errors.New("worker unavailable")
	}
	client := &Client{cmd: cmd, in: stdin, out: bufio.NewReaderSize(stdout, 64*1024), webSeedOrigin: cfg.WebSeedOrigin, trackerOrigin: cfg.TrackerOrigin}
	type startup struct {
		value ready
		err   error
	}
	result := make(chan startup, 1)
	go func() { var v ready; err := client.read(&v); result <- startup{v, err} }()
	select {
	case <-ctx.Done():
		client.abort()
		return nil, errors.New("worker unavailable")
	case r := <-result:
		if r.err != nil || !r.value.Ready || r.value.Backend != "libtorrent" || r.value.Version == "" || !r.value.Policy.V2Only || r.value.Policy.DHT || r.value.Policy.PEX || r.value.Policy.LSD {
			client.abort()
			return nil, errors.New("unsafe worker policy")
		}
	}
	return client, nil
}

func (c *Client) abort() {
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	c.closed = true
}
func (c *Client) read(value any) error {
	line, err := c.out.ReadBytes('\n')
	if err != nil || len(line) > 1<<20 {
		return errors.New("worker unavailable")
	}
	d := json.NewDecoder(strings.NewReader(string(line)))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid worker response")
	}
	return nil
}
func (c *Client) call(op, id string, size int64, hash, trackerURL, webSeedURL, torrentURL string) (response, error) {
	request := map[string]any{"op": op}
	if id != "" {
		request["object_id"] = id
	}
	if op == "add" {
		request["size"] = size
		request["info_hash_v2"] = hash
		request["tracker_url"] = trackerURL
		request["web_seed_url"] = webSeedURL
		request["torrent_url"] = torrentURL
	}
	return c.roundTrip(request)
}

// roundTrip sends one request (the caller supplies all fields but "id") and
// reads its single matching response.
func (c *Client) roundTrip(request map[string]any) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return response{}, errors.New("worker unavailable")
	}
	c.next++
	request["id"] = c.next
	data, err := json.Marshal(request)
	if err != nil {
		return response{}, errors.New("worker unavailable")
	}
	if _, err = c.in.Write(append(data, '\n')); err != nil {
		return response{}, errors.New("worker unavailable")
	}
	var out response
	if err = c.read(&out); err != nil || out.ID != c.next {
		return response{}, errors.New("invalid worker response")
	}
	if out.Error != "" {
		return response{}, mapError(out.Error)
	}
	return out, nil
}
func mapError(code string) error {
	switch code {
	case "not_found":
		return engine.ErrNotFound
	case "conflict":
		return engine.ErrConflict
	case "invalid_transition":
		return engine.ErrTransition
	case "invalid_request":
		return engine.ErrInvalid
	default:
		return errors.New("worker unavailable")
	}
}
func validStatus(s engine.Status) bool {
	if !engine.ValidObjectID(s.ObjectID) || s.TotalBytes < 0 || s.CompletedBytes < 0 || s.CompletedBytes > s.TotalBytes || s.Mock {
		return false
	}
	switch s.State {
	case engine.Downloading, engine.Paused, engine.Cancelled:
		return s.ErrorCode == ""
	case engine.Completed:
		return s.ErrorCode == "" && s.CompletedBytes == s.TotalBytes
	case engine.VerificationFailed:
		return s.ErrorCode == "verification_failed"
	default:
		return false
	}
}
func (c *Client) Add(id string, size int64, hash string) (engine.Status, error) {
	return engine.Status{}, engine.ErrInvalid
}

func (c *Client) AddWithAuthorization(id string, size int64, hash, trackerURL, webSeedURL, torrentURL string) (engine.Status, error) {
	if !engine.ValidObjectID(id) || size < 0 || !engine.LowerHex(hash) {
		return engine.Status{}, engine.ErrInvalid
	}
	if !validTrackerURL(trackerURL, c.trackerOrigin) || !validWebSeedURL(webSeedURL, c.webSeedOrigin, id) || !validTorrentURL(torrentURL, c.webSeedOrigin, id) {
		return engine.Status{}, engine.ErrInvalid
	}
	r, err := c.call("add", id, size, hash, trackerURL, webSeedURL, torrentURL)
	if err != nil {
		return engine.Status{}, err
	}
	if r.Status == nil || !validStatus(*r.Status) || r.Status.ObjectID != id || r.Status.TotalBytes != size {
		return engine.Status{}, errors.New("invalid worker response")
	}
	return *r.Status, nil
}

func validWebSeedURL(value, origin, objectID string) bool {
	if origin == "" || len(value) > 2048 || strings.ContainsAny(value, "%\\") || !strings.HasPrefix(value, origin+"/") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Scheme+"://"+u.Host != origin || u.EscapedPath() != u.Path {
		return false
	}
	prefix := "/api/web-seed/v1/media/" + objectID + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return false
	}
	token := strings.TrimPrefix(u.Path, prefix)
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(decoded) == 0 || base64.RawURLEncoding.EncodeToString(decoded) != part {
			return false
		}
	}
	return true
}
func validTorrentURL(value, origin, objectID string) bool {
	if origin == "" || len(value) > 2048 || strings.ContainsAny(value, "%\\") || !strings.HasPrefix(value, origin+"/") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Scheme+"://"+u.Host != origin || u.EscapedPath() != u.Path {
		return false
	}
	prefix := "/api/torrent/v1/media/" + objectID + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, prefix), ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(decoded) == 0 || base64.RawURLEncoding.EncodeToString(decoded) != part {
			return false
		}
	}
	return true
}
func validTrackerURL(value, origin string) bool {
	if origin == "" || len(value) > 2048 || strings.ContainsAny(value, "%\\") || !strings.HasPrefix(value, origin+"/announce/") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.Scheme+"://"+u.Host != origin || u.EscapedPath() != u.Path {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/announce/"), ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		decoded, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil || len(decoded) == 0 || base64.RawURLEncoding.EncodeToString(decoded) != part {
			return false
		}
	}
	return true
}
func (c *Client) Get(id string) (engine.Status, error) {
	if !engine.ValidObjectID(id) {
		return engine.Status{}, engine.ErrInvalid
	}
	r, err := c.call("get", id, 0, "", "", "", "")
	if err != nil {
		return engine.Status{}, err
	}
	if r.Status == nil || !validStatus(*r.Status) || r.Status.ObjectID != id {
		return engine.Status{}, errors.New("invalid worker response")
	}
	return *r.Status, nil
}
func (c *Client) List() []engine.Status {
	r, err := c.call("list", "", 0, "", "", "", "")
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, s := range r.Transfers {
		if !validStatus(s) || seen[s.ObjectID] {
			return nil
		}
		seen[s.ObjectID] = true
	}
	sort.Slice(r.Transfers, func(i, j int) bool { return r.Transfers[i].ObjectID < r.Transfers[j].ObjectID })
	return r.Transfers
}
func (c *Client) Action(id, action string) (engine.Status, error) {
	if !engine.ValidObjectID(id) || (action != "pause" && action != "resume" && action != "cancel") {
		return engine.Status{}, engine.ErrInvalid
	}
	r, err := c.call(action, id, 0, "", "", "", "")
	if err != nil {
		return engine.Status{}, err
	}
	if r.Status == nil || !validStatus(*r.Status) || r.Status.ObjectID != id {
		return engine.Status{}, errors.New("invalid worker response")
	}
	return *r.Status, nil
}
func validWorkerSeed(s workerSeed) bool {
	return engine.ValidObjectID(s.ObjectID) && engine.ValidSeedState(s.State) && s.TotalBytes >= 0 && s.UploadedBytes >= 0
}

// Seed asks the worker to hash-check and then serve the Agent-finalized
// <object_id>.mp4 in upload-only mode. The file name is derived here, never
// accepted from a caller.
func (c *Client) Seed(id string, size int64, hash, trackerURL, torrentURL string) (engine.SeedStatus, error) {
	if !engine.ValidObjectID(id) || size < 0 || !engine.LowerHex(hash) ||
		!validTrackerURL(trackerURL, c.trackerOrigin) || !validTorrentURL(torrentURL, c.webSeedOrigin, id) {
		return engine.SeedStatus{}, engine.ErrInvalid
	}
	r, err := c.roundTrip(map[string]any{"op": "seed", "object_id": id, "size": size, "info_hash_v2": hash,
		"tracker_url": trackerURL, "torrent_url": torrentURL, "file_name": id + ".mp4"})
	if err != nil {
		return engine.SeedStatus{}, err
	}
	if r.Seed == nil || !validWorkerSeed(*r.Seed) || r.Seed.ObjectID != id || r.Seed.TotalBytes != size {
		return engine.SeedStatus{}, errors.New("invalid worker response")
	}
	return engine.SeedStatus{ObjectID: id, State: r.Seed.State}, nil
}

// Unseed stops serving an object. It never deletes the media file.
func (c *Client) Unseed(id string) (engine.SeedStatus, error) {
	if !engine.ValidObjectID(id) {
		return engine.SeedStatus{}, engine.ErrInvalid
	}
	r, err := c.roundTrip(map[string]any{"op": "unseed", "object_id": id})
	if err != nil {
		return engine.SeedStatus{}, err
	}
	if r.Seed == nil || r.Seed.ObjectID != id || r.Seed.State != engine.SeedRemoved {
		return engine.SeedStatus{}, errors.New("invalid worker response")
	}
	return engine.SeedStatus{ObjectID: id, State: engine.SeedRemoved}, nil
}

// Seeds lists current runtime seeds, sorted by object ID.
func (c *Client) Seeds() ([]engine.SeedStatus, error) {
	r, err := c.roundTrip(map[string]any{"op": "seeds"})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]engine.SeedStatus, 0, len(r.Seeds))
	for _, s := range r.Seeds {
		if !validWorkerSeed(s) || seen[s.ObjectID] {
			return nil, errors.New("invalid worker response")
		}
		seen[s.ObjectID] = true
		out = append(out, engine.SeedStatus{ObjectID: s.ObjectID, State: s.State})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out, nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.next++
	data, _ := json.Marshal(map[string]any{"id": c.next, "op": "shutdown"})
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.abort()
		return errors.New("worker shutdown failed")
	}
	var out response
	if err := c.read(&out); err != nil || out.ID != c.next || !out.OK {
		c.abort()
		return errors.New("worker shutdown failed")
	}
	_ = c.in.Close()
	err := c.cmd.Wait()
	c.closed = true
	if err != nil {
		return errors.New("worker shutdown failed")
	}
	return nil
}
