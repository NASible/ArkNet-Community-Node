package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"arknet/core/internal/admins"
	"arknet/core/internal/nodes"
	"arknet/core/internal/relayrevocations"
)

// The relay list a Node fetches (Prompt 48B):
//
//	{"relay_list": base64url(payload) "." base64url(Ed25519(listKey, relayListDomain || payload))}
//	payload = {"version":1,"issued_at":N,"expires_at":N,"relays":[{"id","address","spki_sha256"}]}
//
// Nodes verify it with the pinned relay-list public key and reject it after
// expires_at. It never carries control URLs or keys.
const (
	relayListDomain   = "ARKNET-RELAY-LIST-V1\x00"
	relayListLifetime = 24 * time.Hour
)

type publishedRelay struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	SPKISHA256 string `json:"spki_sha256"`
}

func signRelayList(cfg *relaysConfig, now time.Time) (string, error) {
	listed := make([]publishedRelay, 0, len(cfg.entries))
	for _, entry := range cfg.entries {
		listed = append(listed, publishedRelay{entry.ID, entry.Address, entry.SPKISHA256})
	}
	payload, err := json.Marshal(struct {
		Version   int              `json:"version"`
		IssuedAt  int64            `json:"issued_at"`
		ExpiresAt int64            `json:"expires_at"`
		Relays    []publishedRelay `json:"relays"`
	}{1, now.Unix(), now.Add(relayListLifetime).Unix(), listed})
	if err != nil {
		return "", err
	}
	signature := ed25519.Sign(cfg.listKey, append([]byte(relayListDomain), payload...))
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// withRelays adds the relay routes in front of Core's handler. Without relay
// configuration it adds nothing.
func withRelays(next http.Handler, cfg *relaysConfig, identities nodes.HandshakeRepository,
	deliveries relayrevocations.Repository, authorizer adminAuthorizer, now func() time.Time) http.Handler {
	if cfg == nil {
		return next
	}
	mux := http.NewServeMux()
	mux.Handle("/", next)
	mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/relays", func(w http.ResponseWriter, r *http.Request) {
		if !authorizeMediaNode(w, r, identities, nodes.CapabilitySwarm) {
			return
		}
		token, err := signRelayList(cfg, now())
		if err != nil {
			http.Error(w, "relay list unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"relay_list": token})
	})
	relayAdmin := newAdminRoutes(http.NewServeMux(), authorizer)
	mux.Handle("GET /api/admin/v1/relays", authenticateAdmin(authorizer, relayAdmin.mux))
	relayAdmin.handle("GET /api/admin/v1/relays", admins.RelaysRead, "", func(w http.ResponseWriter, r *http.Request) {
		type relayView struct {
			ID         string `json:"id"`
			Address    string `json:"address"`
			SPKISHA256 string `json:"spki_sha256"`
			ControlURL string `json:"control_url"`
		}
		type undelivered struct {
			NodeID        string     `json:"node_id"`
			RelayID       string     `json:"relay_id"`
			DecidedAt     time.Time  `json:"decided_at"`
			Attempts      int        `json:"attempts"`
			LastError     string     `json:"last_error"`
			LastAttemptAt *time.Time `json:"last_attempt_at"`
		}
		views := make([]relayView, 0, len(cfg.entries))
		for _, entry := range cfg.entries {
			views = append(views, relayView{entry.ID, entry.Address, entry.SPKISHA256, entry.ControlURL.String()})
		}
		pending := []undelivered{}
		if deliveries != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			tasks, err := deliveries.PendingRelayRevocations(ctx, cfg.ids(), 500)
			if err != nil {
				adminError(w, 503, "relay revocation status unavailable")
				return
			}
			for _, task := range tasks {
				view := undelivered{NodeID: task.NodeID, RelayID: task.RelayID, DecidedAt: task.DecidedAt.UTC(),
					Attempts: task.Attempts, LastError: task.LastError}
				if !task.LastAttemptAt.IsZero() {
					at := task.LastAttemptAt.UTC()
					view.LastAttemptAt = &at
				}
				pending = append(pending, view)
			}
		}
		writeAdminJSON(w, 200, struct {
			Relays      []relayView   `json:"relays"`
			Undelivered []undelivered `json:"undelivered_revocations"`
		}{views, pending})
	})
	return mux
}

func (c *relaysConfig) ids() []string {
	out := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		out = append(out, entry.ID)
	}
	return out
}
