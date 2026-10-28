package globalauth

import (
	"context"

	"arknet/node-agent/internal/enrollment"
)

// Reconcile refreshes the bounded set of still-valid cached memberships after
// authenticated Core connectivity returns. The request contains pairwise IDs
// only and atomically revokes authorizations/sessions Core no longer approves.
func Reconcile(ctx context.Context, repository Repository, identity enrollment.Identity) error {
	if repository == nil {
		return nil
	}
	ids, err := repository.CachedMemberIDs(ctx, MaxCachedMembers)
	if err != nil || len(ids) == 0 {
		return err
	}
	if len(ids) > MaxCachedMembers {
		return ErrInvalidResponse
	}
	states, err := Refresh(ctx, identity, ids)
	if err != nil {
		return err
	}
	return repository.ApplyRefresh(ctx, identity.NodeID, states)
}
