package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/relays"
)

// relayListConfig: Core's relay list is fetched only when the operator pins
// Core's relay-list public key (ARKNET_RELAY_LIST_VERIFY_KEY). The holder
// keeps the newest verified list for the relay connector (Prompt 48D).
type relayListConfig struct {
	key    ed25519.PublicKey
	holder *relays.Holder
}

func loadRelayListConfig(getenv func(string) string, coreURL string) (*relayListConfig, error) {
	value := strings.TrimSpace(getenv("ARKNET_RELAY_LIST_VERIFY_KEY"))
	if value == "" {
		return nil, nil
	}
	if coreURL == "" {
		return nil, fmt.Errorf("ARKNET_RELAY_LIST_VERIFY_KEY requires ARKNET_CORE_URL")
	}
	key, err := base64.StdEncoding.Strict().DecodeString(value)
	// Strict decoding of padded standard base64 accepts only the canonical form.
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ARKNET_RELAY_LIST_VERIFY_KEY must be canonical padded base64 of a 32-byte Ed25519 public key")
	}
	return &relayListConfig{key: ed25519.PublicKey(key), holder: &relays.Holder{}}, nil
}

// refresh runs after an accepted heartbeat. A failure keeps the previous list
// until it expires.
func (c *relayListConfig) refresh(ctx context.Context, identity enrollment.Identity, logger *slog.Logger) error {
	if c == nil {
		return nil
	}
	list, err := relays.Fetch(ctx, identity, c.key)
	if err != nil {
		logger.Warn("relay_list_refresh_failed")
		return err
	}
	c.holder.Store(list)
	return nil
}
