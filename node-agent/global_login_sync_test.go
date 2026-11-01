package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/globalauth"
)

func TestGlobalLoginSyncRegistersCallbacksOnceAndRefreshesKeysPeriodically(t *testing.T) {
	repo := newMemoryGlobalAuth()
	clock := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	registrations, fetches := 0, 0
	var registerErr error
	key := publish(newLoginTest(t).core)
	sync := &globalLoginSync{repo: repo,
		cfg: &globalLoginConfig{coreURL: "https://core.internal", coreLoginURL: testCoreLogin, callbackURLs: []string{testCallbackURL}},
		register: func(_ context.Context, identity enrollment.Identity, urls []string) error {
			registrations++
			if identity.NodeID != thisNode || len(urls) != 1 || urls[0] != testCallbackURL {
				t.Errorf("registration %v %v", identity.NodeID, urls)
			}
			return registerErr
		},
		fetch: func(context.Context, string) ([]globalauth.VerificationKey, error) { fetches++; return key, nil },
		now:   func() time.Time { return clock }}
	identity := enrollment.Identity{NodeID: thisNode, Status: "APPROVED"}
	registerErr = globalauth.ErrUnavailable
	if err := sync.Run(context.Background(), identity); !errors.Is(err, globalauth.ErrUnavailable) {
		t.Fatalf("failed registration not reported: %v", err)
	}
	registerErr = nil
	if err := sync.Run(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := sync.Run(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if registrations != 2 || fetches != 1 || len(repo.keys) != 1 {
		t.Fatalf("registrations=%d fetches=%d keys=%d", registrations, fetches, len(repo.keys))
	}
	clock = clock.Add(globalLoginKeyRefresh)
	if err := sync.Run(context.Background(), identity); err != nil || fetches != 2 {
		t.Fatalf("keys not refreshed after the interval: fetches=%d %v", fetches, err)
	}
	var disabled *globalLoginSync
	if err := disabled.Run(context.Background(), identity); err != nil {
		t.Fatal("disabled sync must be a no-op")
	}
}
