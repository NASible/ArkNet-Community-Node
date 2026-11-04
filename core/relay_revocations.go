package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"arknet/core/internal/relayrevocations"
)

const (
	relayRevocationPath      = "/api/relay/v1/revocations"
	relayRevocationBatch     = 500
	relayRevocationMaxDelay  = time.Minute
	relayRevocationPassEvery = 2 * time.Second
)

// relayRevocationPusher delivers each BLOCKED Node's revocation to every
// configured relay and retries, with bounded backoff, until each relay
// acknowledges. Each message is freshly signed (the relay accepts only
// revocations issued within 60 s, each ID once) with that relay's own key.
type relayRevocationPusher struct {
	relays  *relaysConfig
	repo    relayrevocations.Repository
	tracker *trackerGrantIssuer
	now     func() time.Time
	logger  *slog.Logger
	client  *http.Client
}

func newRelayRevocationPusher(relays *relaysConfig, repo relayrevocations.Repository, tracker *trackerGrantIssuer,
	now func() time.Time, logger *slog.Logger) *relayRevocationPusher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &relayRevocationPusher{relays: relays, repo: repo, tracker: tracker, now: now, logger: logger,
		client: &http.Client{Transport: transport, Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// relayRevocationBackoff is the wait after the given number of failed attempts.
func relayRevocationBackoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	if attempts > 7 {
		return relayRevocationMaxDelay
	}
	delay := time.Second << (attempts - 1)
	if delay > relayRevocationMaxDelay {
		return relayRevocationMaxDelay
	}
	return delay
}

func (p *relayRevocationPusher) run(ctx context.Context) {
	ticker := time.NewTicker(relayRevocationPassEvery)
	defer ticker.Stop()
	for {
		if err := p.once(ctx); err != nil && ctx.Err() == nil {
			p.logger.Warn("relay_revocation_pass_failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// once makes one delivery attempt for every due task.
func (p *relayRevocationPusher) once(ctx context.Context) error {
	tasks, err := p.repo.PendingRelayRevocations(ctx, p.relays.ids(), relayRevocationBatch)
	if err != nil {
		return err
	}
	now := p.now()
	due := map[string][]relayrevocations.Task{}
	for _, task := range tasks {
		if task.Attempts > 0 && task.LastError != "" && now.Before(task.LastAttemptAt.Add(relayRevocationBackoff(task.Attempts))) {
			continue
		}
		due[task.RelayID] = append(due[task.RelayID], task)
	}
	if len(due) == 0 {
		return nil
	}
	hashes, err := p.repo.MediaInfoHashes(ctx)
	if err != nil {
		return err
	}
	decoded := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 32 {
			continue
		}
		decoded = append(decoded, raw)
	}
	// Relays are independent: one relay's outage never delays another's revocations.
	var wg sync.WaitGroup
	errs := make(chan error, len(p.relays.entries))
	for _, entry := range p.relays.entries {
		if len(due[entry.ID]) == 0 {
			continue
		}
		wg.Add(1)
		go func(entry relayEntry, tasks []relayrevocations.Task) {
			defer wg.Done()
			for _, task := range tasks {
				failure := p.deliver(ctx, entry, task.NodeID, decoded)
				if err := p.repo.RecordRelayRevocationAttempt(ctx, task.NodeID, entry.ID, failure); err != nil {
					errs <- err
					return
				}
				if failure == "" {
					p.logger.Info("relay_revocation_delivered", "relay_id", entry.ID, "attempts", task.Attempts+1)
				} else {
					p.logger.Warn("relay_revocation_failed", "relay_id", entry.ID, "reason", failure, "attempts", task.Attempts+1)
				}
			}
		}(entry, due[entry.ID])
	}
	wg.Wait()
	close(errs)
	return <-errs
}

// deliver sends every subject of node, in bounded messages, and returns ""
// once the relay acknowledged all of them, or a short failure code.
func (p *relayRevocationPusher) deliver(ctx context.Context, entry relayEntry, nodeID string, hashes [][]byte) string {
	subjects := make([][16]byte, 0, len(hashes))
	for _, hash := range hashes {
		subjects = append(subjects, p.tracker.subject(nodeID, hash))
	}
	for start := 0; start < len(subjects); start += maxRelayRevocationSubjects {
		end := min(start+maxRelayRevocationSubjects, len(subjects))
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return "internal"
		}
		token, err := signRelayRevocation(entry.revocationKey, entry.ID, id, p.now(), subjects[start:end])
		if err != nil {
			return "internal"
		}
		if failure := p.post(ctx, entry, token); failure != "" {
			return failure
		}
	}
	return ""
}

func (p *relayRevocationPusher) post(ctx context.Context, entry relayEntry, token string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(entry.ControlURL.String(), "/")+relayRevocationPath,
		strings.NewReader(token))
	if err != nil {
		return "internal"
	}
	request.Header.Set("Content-Type", "text/plain")
	response, err := p.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "timeout"
		}
		return "unreachable"
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	switch response.StatusCode {
	case http.StatusNoContent, http.StatusConflict: // 409: this message was already applied
		return ""
	default:
		return fmt.Sprintf("status_%d", response.StatusCode)
	}
}
