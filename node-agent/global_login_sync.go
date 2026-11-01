package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
)

// globalLoginKeyRefresh is how often a CONNECTED Agent re-reads Core's
// assertion key set (Core marks it cacheable for one hour). Logins also read
// it, so a rotation is picked up at the next login start at the latest.
const globalLoginKeyRefresh = 10 * time.Minute

// globalLoginSync runs after each accepted heartbeat: it registers the member
// UI callback URLs with Core once per process and keeps the persisted key set
// fresh, so offline login has current keys when Core becomes unreachable.
type globalLoginSync struct {
	cfg      *globalLoginConfig
	repo     globalauth.Repository
	register func(context.Context, enrollment.Identity, []string) error
	fetch    func(context.Context, string) ([]globalauth.VerificationKey, error)
	now      func() time.Time

	mu         sync.Mutex
	registered bool
	keysAt     time.Time
}

func newGlobalLoginSync(cfg *globalLoginConfig, repo globalauth.Repository) *globalLoginSync {
	if cfg == nil || repo == nil {
		return nil
	}
	return &globalLoginSync{cfg: cfg, repo: repo, register: globalauth.RegisterLoginCallbacks, fetch: globalauth.FetchAssertionKeys, now: time.Now}
}

func (s *globalLoginSync) Run(ctx context.Context, identity enrollment.Identity) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	if !s.registered {
		if err := s.register(ctx, identity, s.cfg.callbackURLs); err != nil {
			errs = append(errs, err)
		} else {
			s.registered = true
		}
	}
	if s.keysAt.IsZero() || !s.now().Before(s.keysAt.Add(globalLoginKeyRefresh)) {
		keys, err := s.fetch(ctx, s.cfg.coreURL)
		if err == nil {
			err = s.repo.ReplaceAssertionKeys(ctx, keys)
		}
		if err != nil {
			errs = append(errs, err)
		} else {
			s.keysAt = s.now()
		}
	}
	return errors.Join(errs...)
}
