package main

import (
	"arknet/core/internal/media"
	"arknet/core/internal/memberships"
	"arknet/core/internal/nodes"
	"arknet/core/internal/routing"
	"arknet/core/internal/store"
	"arknet/core/internal/users"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// version is the whole-stack release marker; TestVersionMatchesStack guards drift.
const version = "0.0.1-dev"

type databaseHealth interface{ Ping(context.Context) error }

func newHandler(database databaseHealth, repositories ...nodes.EnrollmentRepository) http.Handler {
	return newHandlerWithTracker(database, nil, repositories...)
}

func newHandlerWithTracker(database databaseHealth, tracker *trackerGrantIssuer, repositories ...nodes.EnrollmentRepository) http.Handler {
	return newHandlerWithWebSeed(database, tracker, nil, repositories...)
}

func newHandlerWithWebSeed(database databaseHealth, tracker *trackerGrantIssuer, webSeed *webSeedConfig, repositories ...nodes.EnrollmentRepository) http.Handler {
	var catalogStore store.MediaCatalogRepository
	var replicaStore store.MediaReplicaRepository
	for _, repository := range repositories {
		if candidate, ok := repository.(store.MediaCatalogRepository); ok && catalogStore == nil {
			catalogStore = candidate
		}
		if candidate, ok := repository.(store.MediaReplicaRepository); ok && replicaStore == nil {
			replicaStore = candidate
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth(database))
	mux.HandleFunc("GET /api/federation/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"version\":\"" + version + "\"}\n"))
	})
	var repository nodes.EnrollmentRepository
	if len(repositories) > 0 {
		repository = repositories[0]
	}
	mux.HandleFunc("POST /api/federation/v1/enrollments", enrollmentHandler(repository))
	handshake, _ := repository.(nodes.HandshakeRepository)
	mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/enrollment", enrollmentStatusHandler(handshake))
	heartbeats, _ := repository.(nodes.HeartbeatRepository)
	mux.HandleFunc("POST /api/federation/v1/nodes/{node_id}/heartbeats", heartbeatHandler(heartbeats))
	identities, _ := repository.(users.Repository)
	if identities != nil {
		registerIdentityRoutes(mux, identities)
	}
	if invitations, ok := repository.(memberships.Repository); ok {
		mux.HandleFunc("POST /api/federation/v1/nodes/{node_id}/invitations", invitationCreateHandler(invitations, handshake))
		if identities != nil {
			mux.HandleFunc("POST /api/identity/v1/invitations/accept", invitationAcceptHandler(invitations, identities))
		}
	}
	if homes, ok := repository.(memberships.HomeNodeRepository); ok && identities != nil {
		registerHomeNodeRoutes(mux, identities, homes)
		if routes, ok := repository.(routing.Repository); ok && catalogStore != nil {
			missRepository, _ := repository.(routing.MissRepository)
			var misses *missDispatcher
			if missRepository != nil {
				misses = newMissDispatcher(missRepository)
				mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/media-misses", mediaMissesHandler(missRepository, handshake))
			}
			mux.HandleFunc("GET /api/routing/v1/media/{media_id}", mediaRoutingHandler(catalogStore, identities, homes, routes, misses))
		}
	}
	if authorizations, ok := repository.(memberships.OfflineAuthorizationRepository); ok {
		mux.HandleFunc("POST /api/federation/v1/nodes/{node_id}/offline-authorizations", globalAuthorizationHandler(authorizations, handshake))
		mux.HandleFunc("POST /api/federation/v1/nodes/{node_id}/offline-authorizations/refresh", globalAuthorizationRefreshHandler(authorizations, handshake))
	}

	if catalogStore != nil {
		mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/media", mediaListHandler(catalogStore, handshake))
		mux.HandleFunc("GET /api/federation/v1/nodes/{node_id}/media/{id}", mediaGetHandler(catalogStore, handshake))
		grants := trackerGrantHandlerWithWebSeed(catalogStore, handshake, tracker, webSeed)
		for _, candidate := range repositories {
			if sources, ok := candidate.(seedSourceAuthority); ok {
				grants = trackerGrantHandlerWithSources(catalogStore, handshake, tracker, webSeed, sources)
				break
			}
		}
		mux.HandleFunc("POST /api/federation/v1/nodes/{node_id}/media/{id}/tracker-grants", grants)
		if replicaStore != nil {
			mux.HandleFunc("PUT /api/federation/v1/nodes/{node_id}/media/{id}/replicas", mediaReplicaHandler(catalogStore, replicaStore, handshake))
		}
		if webSeed != nil {
			mux.HandleFunc("GET /api/web-seed/v1/media/{id}/{grant}", webSeedHandler(catalogStore, tracker, webSeed))
			// libtorrent appends the torrent name for tail-padded v2 storage; only the exact media ID is accepted.
			mux.HandleFunc("GET /api/web-seed/v1/media/{id}/{grant}/{name}", webSeedHandler(catalogStore, tracker, webSeed))
			mux.HandleFunc("GET /api/torrent/v1/media/{id}/{grant}", torrentMetadataHandler(catalogStore, tracker, webSeed))
		}
	}
	return mux
}

// handleHealth returns a health response for the /health endpoint.
func handleHealth(db databaseHealth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		healthy := db != nil && db.Ping(ctx) == nil
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(struct {
			Healthy         bool `json:"healthy"`
			DatabaseHealthy bool `json:"database_healthy"`
		}{healthy, healthy})
	}
}

// authorizeMediaNode authenticates a read as the approved node named in its
// node-scoped route. Catalog reads are never available through a global,
// unauthenticated endpoint.
func authorizeMediaNode(w http.ResponseWriter, r *http.Request, repo nodes.HandshakeRepository) bool {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("node_id")
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, body, 1))
	if err != nil || len(data) != 0 || !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
		unauthorizedNode(w)
		return false
	}
	if repo == nil {
		http.Error(w, "media catalog unavailable", http.StatusServiceUnavailable)
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	node, key, err := repo.EnrollmentIdentity(ctx, id)
	if errors.Is(err, nodes.ErrNotFound) || errors.Is(err, nodes.ErrInvalidPublicID) {
		unauthorizedNode(w)
		return false
	}
	if err != nil {
		http.Error(w, "media catalog unavailable", http.StatusServiceUnavailable)
		return false
	}
	if node.PublicID != id || !verifyNodeProof(r, nil, id, key) {
		unauthorizedNode(w)
		return false
	}
	if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
		http.Error(w, "node is not approved", http.StatusForbidden)
		return false
	}
	return true
}

// mediaListHandler handles the authenticated node catalog endpoint.
func mediaListHandler(repo store.MediaCatalogRepository, identities nodes.HandshakeRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusMethodNotAllowed)
			})).ServeHTTP(w, r)
			return
		}

		if !authorizeMediaNode(w, r, identities) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		const (
			page  = 1
			limit = 100
		)
		objects, err := repo.ListMediaObjects(r.Context(), true, limit)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "media_catalog_unavailable"})
			return
		}

		result := make([]media.APIObject, len(objects))
		for i, object := range objects {
			result[i] = object.AsAPIView()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"objects": result,
			"page":    page,
			"limit":   limit,
		})
	}
}

// mediaGetHandler handles an authenticated node lookup by opaque media ID.
func mediaGetHandler(repo store.MediaCatalogRepository, identities nodes.HandshakeRepository) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if !authorizeMediaNode(w, r, identities) {
			return
		}
		id := r.PathValue("id")
		if !media.ValidMediaID(id) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid media ID"})
			return
		}

		obj, err := repo.GetMediaObjectByPublicID(r.Context(), id)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			if errors.Is(err, store.ErrNotFound) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "media object not found"})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "media_catalog_unavailable"})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj.AsAPIView())
	}
}

func trackerGrantHandler(repo store.MediaCatalogRepository, identities nodes.HandshakeRepository, issuer *trackerGrantIssuer) http.HandlerFunc {
	return trackerGrantHandlerWithWebSeed(repo, identities, issuer, nil)
}

// seedSourceAuthority is Core's authoritative answer to "may this Node serve
// this object to peers": a verified replica, Peer Serving opt-in, approved and
// trusted Node, and an ACTIVE object. Without it seed grants are never issued.
type seedSourceAuthority interface {
	MediaSourceEligible(ctx context.Context, nodeID, mediaID string) (bool, error)
}

func trackerGrantHandlerWithWebSeed(repo store.MediaCatalogRepository, identities nodes.HandshakeRepository, issuer *trackerGrantIssuer, webSeed *webSeedConfig) http.HandlerFunc {
	return trackerGrantHandlerWithSources(repo, identities, issuer, webSeed, nil)
}

func trackerGrantHandlerWithSources(repo store.MediaCatalogRepository, identities nodes.HandshakeRepository, issuer *trackerGrantIssuer, webSeed *webSeedConfig, sources seedSourceAuthority) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, authorized := authorizeTrackerGrantNode(w, r, identities)
		if !authorized {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		actionMask, actions, err := parseTrackerGrantActions(body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid tracker grant actions"})
			return
		}
		id := r.PathValue("id")
		if !media.ValidMediaID(id) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid media ID"})
			return
		}
		obj, err := repo.GetMediaObjectByPublicID(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "media object not found"})
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "media_catalog_unavailable"})
			return
		}
		if obj.State != media.ObjectActive || !validInfoHashV2(obj.InfoHashV2) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "media object is not tracker-ready"})
			return
		}
		if issuer == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "tracker grants unavailable"})
			return
		}
		if actionMask&trackerGrantActionSeed != 0 {
			eligible := false
			if sources != nil {
				eligible, err = sources.MediaSourceEligible(r.Context(), r.PathValue("node_id"), id)
				if err != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "tracker grants unavailable"})
					return
				}
			}
			if !eligible {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "node is not an eligible source"})
				return
			}
		}
		grant, expiresAt, err := issuer.issueGrant(r.PathValue("node_id"), obj.InfoHashV2, actionMask)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "tracker grants unavailable"})
			return
		}
		tracker := *issuer.announceBase
		tracker.Path = "/announce/" + grant
		var webSeedURL, torrentURL string
		if webSeed != nil {
			if actionMask&trackerGrantActionDownload != 0 {
				seed := *webSeed.origin
				seed.Path = "/api/web-seed/v1/media/" + id + "/" + grant
				webSeedURL = seed.String()
			}
			// Torrent metadata (never payload) for both receiving and authorized serving.
			metadata := *webSeed.origin
			metadata.Path = "/api/torrent/v1/media/" + id + "/" + grant
			torrentURL = metadata.String()
		}
		_ = json.NewEncoder(w).Encode(struct {
			TrackerURL string   `json:"tracker_url"`
			ExpiresAt  string   `json:"expires_at"`
			Actions    []string `json:"actions"`
			WebSeedURL string   `json:"web_seed_url,omitempty"`
			TorrentURL string   `json:"torrent_url,omitempty"`
		}{TrackerURL: tracker.String(), ExpiresAt: expiresAt.Format(time.RFC3339), Actions: actions, WebSeedURL: webSeedURL, TorrentURL: torrentURL})
	}
}

func authorizeTrackerGrantNode(w http.ResponseWriter, r *http.Request, repo nodes.HandshakeRepository) ([]byte, bool) {
	w.Header().Set("Cache-Control", "no-store")
	id := r.PathValue("node_id")
	body := r.Body
	if body == nil {
		body = http.NoBody
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, body, 256))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid tracker grant actions"})
		return nil, false
	}
	if !nodes.ValidPublicID(id) || r.URL.RawQuery != "" || r.URL.ForceQuery {
		unauthorizedNode(w)
		return nil, false
	}
	if repo == nil {
		http.Error(w, "media catalog unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	node, key, err := repo.EnrollmentIdentity(ctx, id)
	if errors.Is(err, nodes.ErrNotFound) || errors.Is(err, nodes.ErrInvalidPublicID) {
		unauthorizedNode(w)
		return nil, false
	}
	if err != nil {
		http.Error(w, "media catalog unavailable", http.StatusServiceUnavailable)
		return nil, false
	}
	if node.PublicID != id || !verifyNodeProof(r, data, id, key) {
		unauthorizedNode(w)
		return nil, false
	}
	if node.EnrollmentStatus != nodes.EnrollmentApproved || node.TrustStatus != nodes.TrustTrusted {
		http.Error(w, "node is not approved", http.StatusForbidden)
		return nil, false
	}
	return data, true
}

func parseTrackerGrantActions(body []byte) (byte, []string, error) {
	if len(body) == 0 {
		return trackerGrantActionDownload, []string{"download"}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return 0, nil, errors.New("tracker grant body must be an object")
	}
	seenActions := false
	var mask byte
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || name != "actions" || seenActions {
			return 0, nil, errors.New("tracker grant body must contain exactly one actions field")
		}
		seenActions = true
		var requested []string
		if err := decoder.Decode(&requested); err != nil || len(requested) == 0 {
			return 0, nil, errors.New("tracker grant actions must be a non-empty string array")
		}
		for _, action := range requested {
			var bit byte
			switch action {
			case "download":
				bit = trackerGrantActionDownload
			case "seed":
				bit = trackerGrantActionSeed
			default:
				return 0, nil, errors.New("unknown tracker grant action")
			}
			if mask&bit != 0 {
				return 0, nil, errors.New("duplicate tracker grant action")
			}
			mask |= bit
		}
	}
	if _, err := decoder.Token(); err != nil || !seenActions {
		return 0, nil, errors.New("tracker grant body must contain exactly one actions field")
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return 0, nil, errors.New("trailing tracker grant body")
	}
	actions := make([]string, 0, 2)
	if mask&trackerGrantActionDownload != 0 {
		actions = append(actions, "download")
	}
	if mask&trackerGrantActionSeed != 0 {
		actions = append(actions, "seed")
	}
	return mask, actions, nil
}
