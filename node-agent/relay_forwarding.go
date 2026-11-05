package main

import (
	"time"

	"arknet/node-agent/internal/transfer"
)

func (cfg config) mediaOptions(repository mediaStorage) mediaOpt {
	media := mediaOpt{repository: repository, allocation: cfg.mediaStorageAllocation, coreURL: cfg.coreURL, mediaDir: cfg.mediaDir}
	if cfg.relayList != nil {
		media.relayList = cfg.relayList.holder
	}
	return media
}

// currentRelays snapshots only the newest unexpired, signature-verified Core
// list. No list means no peer transport; the Core web-seed remains independent.
func (m mediaOpt) currentRelays(now time.Time) ([]transfer.Relay, int64) {
	list, ok := m.relayList.Current(now)
	if !ok || len(list.Relays) == 0 {
		return nil, 0
	}
	entries := make([]transfer.Relay, len(list.Relays))
	for i, r := range list.Relays {
		entries[i] = transfer.Relay{ID: r.ID, Address: r.Address, SPKISHA256: r.SPKISHA256}
	}
	return entries, list.ExpiresAt.Unix()
}
