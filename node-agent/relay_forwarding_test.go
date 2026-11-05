package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/mediamiss"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/relays"
	"arknet/node-agent/internal/transfer"
)

func verifiedForwardList(t *testing.T, issued, expires time.Time, id string) relays.List {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"version": 1, "issued_at": issued.Unix(), "expires_at": expires.Unix(), "relays": []relays.Relay{{ID: id, Address: id + ".example:443", SPKISHA256: strings.Repeat("a", 64)}}})
	token := base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte(relays.Domain), payload...)))
	list, err := relays.Verify(token, pub, issued)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func assertForwarded(t *testing.T, got []transfer.Relay, expiry int64, want *relays.List) {
	t.Helper()
	if want == nil {
		if got != nil || expiry != 0 {
			t.Fatalf("no verified list must forward nil/0, got %+v/%d", got, expiry)
		}
		return
	}
	expected := []transfer.Relay{{ID: want.Relays[0].ID, Address: want.Relays[0].Address, SPKISHA256: want.Relays[0].SPKISHA256}}
	if !reflect.DeepEqual(got, expected) || expiry != want.ExpiresAt.Unix() {
		t.Fatalf("forwarded %+v/%d, want %+v/%d", got, expiry, expected, want.ExpiresAt.Unix())
	}
}

func TestSeedRelayListExpiringDuringGrantIsNotForwarded(t *testing.T) {
	h := newSeedHarness(t)
	holder := &relays.Holder{}
	expiry := h.clock.Add(time.Minute)
	holder.Store(verifiedForwardList(t, h.clock, expiry, "relay"))
	h.reconciler.media.relayList = holder
	grant := h.reconciler.seedGrant
	h.reconciler.seedGrant = func(ctx context.Context, identity enrollment.Identity, id string) (catalog.TransferGrant, error) {
		result, err := grant(ctx, identity, id)
		h.clock = expiry
		return result, err
	}
	if err := h.once(); err != nil {
		t.Fatal(err)
	}
	req := h.service.seedReqs[0]
	assertForwarded(t, req.Relays, req.RelayListExpiresAt, nil)
}

func TestConfiguredMediaUsesLiveOptInRelayHolder(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	holder := &relays.Holder{}
	cfg := config{relayList: &relayListConfig{holder: holder}}
	media := cfg.mediaOptions(queuedTransferRepository())
	assertForwarded(t, func() []transfer.Relay { r, _ := media.currentRelays(now); return r }(), 0, nil)
	list := verifiedForwardList(t, now, now.Add(time.Hour), "relay")
	holder.Store(list)
	entries, expiry := media.currentRelays(now)
	assertForwarded(t, entries, expiry, &list)
	off := config{}
	media = off.mediaOptions(queuedTransferRepository())
	entries, expiry = media.currentRelays(now)
	assertForwarded(t, entries, expiry, nil)
}

func TestRelayForwardingStartAndResumeUseLatestVerifiedHolder(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	holder := &relays.Holder{}
	first := verifiedForwardList(t, now.Add(-time.Minute), now.Add(time.Hour), "first")
	latest := verifiedForwardList(t, now, now.Add(2*time.Hour), "latest")
	holder.Store(first)
	repo := queuedTransferRepository()
	service := &fakeTransferService{}
	media := mediaOpt{repository: repo, coreURL: "https://core.example", relayList: holder,
		fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
			return []catalog.Object{catalogTestObject()}, nil
		},
		grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
			return catalog.TransferGrant{TrackerURL: "https://tracker.example/announce/grant", WebSeedURL: "https://core.example/web-seed", TorrentURL: "https://core.example/torrent"}, nil
		}}
	h := newRoutes(testRepository{}, media, transferOpt{service: service})
	transferAction(t, h, "start", 200)
	assertForwarded(t, service.requests[0].Relays, service.requests[0].RelayListExpiresAt, &first)
	holder.Store(latest)
	holder.Store(first)
	transferAction(t, h, "resume", 200)
	assertForwarded(t, service.refreshes[0].Relays, service.refreshes[0].RelayListExpiresAt, &latest)
	// Forwarded request storage must not alias the trusted holder's slice.
	service.refreshes[0].Relays[0].Address = "evil.example:1"
	transferAction(t, h, "resume", 200)
	assertForwarded(t, service.refreshes[1].Relays, service.refreshes[1].RelayListExpiresAt, &latest)
	for _, hld := range []*relays.Holder{nil, {}, func() *relays.Holder {
		h := &relays.Holder{}
		h.Store(verifiedForwardList(t, now.Add(-time.Hour), now, "expired"))
		return h
	}()} {
		media.relayList = hld
		h = newRoutes(testRepository{}, media, transferOpt{service: service})
		transferAction(t, h, "start", 200)
		req := service.requests[len(service.requests)-1]
		assertForwarded(t, req.Relays, req.RelayListExpiresAt, nil)
		if req.WebSeedURL == "" {
			t.Fatal("missing web-seed fallback")
		}
		transferAction(t, h, "resume", 200)
		req = service.refreshes[len(service.refreshes)-1]
		assertForwarded(t, req.Relays, req.RelayListExpiresAt, nil)
	}
}

func TestRelayForwardingReconcilersAndAutomaticMiss(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	first := verifiedForwardList(t, now.Add(-time.Minute), now.Add(time.Hour), "first")
	latest := verifiedForwardList(t, now, now.Add(2*time.Hour), "latest")
	for _, mode := range []string{"valid", "absent", "expired"} {
		t.Run(mode, func(t *testing.T) {
			holder := &relays.Holder{}
			var want *relays.List
			switch mode {
			case "valid":
				holder.Store(first)
				want = &first
			case "expired":
				holder.Store(verifiedForwardList(t, now.Add(-time.Hour), now, "expired"))
			case "absent":
				holder = nil
			}
			h := newSeedHarness(t)
			h.clock = now
			h.reconciler.media.relayList = holder
			if err := h.once(); err != nil {
				t.Fatal(err)
			}
			seed := h.service.seedReqs[0]
			assertForwarded(t, seed.Relays, seed.RelayListExpiresAt, want)
			if mode == "valid" {
				holder.Store(latest)
				holder.Store(first)
				want = &latest
			}
			h.clock = h.clock.Add(h.lifetime / 2)
			if err := h.once(); err != nil {
				t.Fatal(err)
			}
			seed = h.service.seedReqs[1]
			assertForwarded(t, seed.Relays, seed.RelayListExpiresAt, want)

			miss := newMissHarness(t, 1<<30, []catalog.Miss{{MediaID: missA, MissCount: 1}})
			miss.misses.policy = mediamiss.Automatic
			miss.syncer.media.relayList = holder
			miss.syncer.transfers.media.relayList = holder
			if err := miss.syncer.once(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(miss.service.requests) != 1 {
				t.Fatal("automatic miss did not start")
			}
			request := miss.service.requests[0]
			assertForwarded(t, request.Relays, request.RelayListExpiresAt, want)
			if request.WebSeedURL == "" {
				t.Fatal("automatic miss lost web-seed")
			}

			for _, state := range []transfer.State{transfer.Downloading, transfer.Paused} {
				repo := queuedTransferRepository()
				repo.objects[0].LocalState = mediaselection.Downloading
				service := &fakeTransferService{real: true, status: transfer.Status{ObjectID: selectionID, TotalBytes: 4096, State: state}}
				object := catalogTestObject()
				object.InfoHashV2 = strings.Repeat("b", 64)
				reconciler := transferReconciler{service: service, media: mediaOpt{repository: repo, coreURL: "https://core.example", relayList: holder,
					fetchCatalog: func(context.Context, enrollment.Identity) ([]catalog.Object, error) {
						return []catalog.Object{object}, nil
					},
					grantTransfer: func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error) {
						return catalog.TransferGrant{TrackerURL: "https://tracker.example/announce/grant", WebSeedURL: "https://core.example/web-seed", TorrentURL: "https://core.example/torrent"}, nil
					}}}
				if err := reconciler.once(context.Background()); err != nil {
					t.Fatal(err)
				}
				request = service.refreshes[0]
				assertForwarded(t, request.Relays, request.RelayListExpiresAt, want)
				if request.WebSeedURL == "" {
					t.Fatal("reconciler lost web-seed")
				}
			}
		})
	}
}
