package main

import (
	"context"
	"errors"
	"sort"
	"time"

	"arknet/node-agent/internal/catalog"
	"arknet/node-agent/internal/enrollment"
	"arknet/node-agent/internal/localstate"
	"arknet/node-agent/internal/mediaselection"
	"arknet/node-agent/internal/transfer"
)

// seedReconciler makes local serving follow the operator's Peer Serving
// opt-in and Core's authorization, one reconcile interval at a time:
//
//   - it serves only while Peer Serving is ON, the Node is CONNECTED with an
//     APPROVED identity, and Core keeps issuing a seed grant for the object;
//   - it serves only AVAILABLE local copies of ACTIVE catalog objects with a
//     v2 hash, and the transfer service still hash-checks every piece;
//   - anything else (OFF, unreadable setting, not CONNECTED/REVOKED, removed
//     or unavailable media, Core 401/403, or an expired grant) is unseeded.
//
// Seed grants are refreshed at half their lifetime. A transient Core failure
// keeps an existing seed only until its current grant expires.
type seedReconciler struct {
	seeds       transfer.Seeder
	media       mediaOpt
	peerServing func(context.Context) (bool, error)
	nodeState   func(context.Context) (localstate.State, error)
	seedGrant   func(context.Context, enrollment.Identity, string) (catalog.TransferGrant, error)
	now         func() time.Time
	windows     map[string]seedWindow
}

type seedWindow struct{ refreshAt, expiresAt time.Time }

func (r *seedReconciler) once(ctx context.Context) error {
	if r.seeds == nil {
		return nil
	}
	if r.windows == nil {
		r.windows = map[string]seedWindow{}
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	current, err := r.seeds.ListSeeds(ctx)
	if err != nil {
		return err
	}
	serving := make(map[string]transfer.SeedState, len(current))
	for _, s := range current {
		serving[s.ObjectID] = s.State
	}
	identity, allowed, err := r.authorized(ctx)
	if !allowed {
		return errors.Join(err, r.unseedWhere(ctx, serving, func(string) bool { return true }))
	}
	candidates, err := r.candidates(ctx)
	if err != nil {
		// Without Core confirming the catalog, never start or refresh a seed;
		// existing ones last only as long as their current grant.
		return errors.Join(err, r.unseedWhere(ctx, serving, func(id string) bool { return !now.Before(r.windows[id].expiresAt) }))
	}
	var errs []error
	errs = append(errs, r.unseedWhere(ctx, serving, func(id string) bool { _, ok := candidates[id]; return !ok }))
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		state, seeding := serving[id]
		if state == transfer.SeedVerificationFailed {
			continue // the local copy failed its hash check; do not recheck it every interval
		}
		window, granted := r.windows[id]
		if seeding && granted && now.Before(window.refreshAt) {
			continue
		}
		grant, err := r.seedGrant(ctx, identity, id)
		if errors.Is(err, catalog.ErrGrantRefused) {
			errs = append(errs, r.unseedWhere(ctx, serving, func(candidate string) bool { return candidate == id }))
			continue
		}
		if err != nil {
			errs = append(errs, err)
			if seeding && !now.Before(window.expiresAt) {
				errs = append(errs, r.unseedWhere(ctx, serving, func(candidate string) bool { return candidate == id }))
			}
			continue
		}
		object := candidates[id]
		if _, err := r.seeds.Seed(ctx, transfer.SeedRequest{ObjectID: id, Size: object.Size, InfoHashV2: object.InfoHashV2,
			TrackerURL: grant.TrackerURL, TorrentURL: grant.TorrentURL}); err != nil {
			errs = append(errs, err)
			continue
		}
		r.windows[id] = seedWindow{refreshAt: now.Add(grant.ExpiresAt.Sub(now) / 2), expiresAt: grant.ExpiresAt}
	}
	return errors.Join(errs...)
}

// authorized reports whether this Node may serve at all. Unknown means no.
func (r *seedReconciler) authorized(ctx context.Context) (enrollment.Identity, bool, error) {
	enabled, err := r.peerServing(ctx)
	if err != nil || !enabled {
		return enrollment.Identity{}, false, err
	}
	state, err := r.nodeState(ctx)
	if err != nil || state != localstate.Connected {
		return enrollment.Identity{}, false, err
	}
	identity, err := r.media.identity(ctx)
	if err != nil {
		return enrollment.Identity{}, false, nil
	}
	return identity, true, nil
}

// candidates are AVAILABLE local copies of ACTIVE Core objects whose sizes
// agree and which carry a v2 info hash.
func (r *seedReconciler) candidates(ctx context.Context) (map[string]catalog.Object, error) {
	if r.media.repository == nil {
		return nil, errors.New("local media unavailable")
	}
	selections, err := r.media.repository.ListMediaSelections(ctx)
	if err != nil {
		return nil, err
	}
	objects, err := r.media.catalog(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]catalog.Object, len(objects))
	for _, object := range objects {
		byID[object.ID] = object
	}
	out := map[string]catalog.Object{}
	for _, selection := range selections {
		object, ok := byID[selection.ID]
		if !ok || selection.LocalState != mediaselection.Available || selection.State != catalog.Active ||
			object.State != catalog.Active || object.Size != selection.Size || !validInfoHash(object.InfoHashV2) {
			continue
		}
		out[selection.ID] = object
	}
	return out, nil
}

func validInfoHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (r *seedReconciler) unseedWhere(ctx context.Context, serving map[string]transfer.SeedState, match func(string) bool) error {
	var errs []error
	for id := range serving {
		if !match(id) {
			continue
		}
		if _, err := r.seeds.Unseed(ctx, id); err != nil && !errors.Is(err, transfer.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		delete(serving, id)
		delete(r.windows, id)
	}
	return errors.Join(errs...)
}
