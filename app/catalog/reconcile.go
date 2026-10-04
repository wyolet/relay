package catalog

import (
	"context"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
)

// commitWithGrants recomputes the per-policy allowed-combo sets (a function of
// providers + hosts + models + policies) then atomically publishes the clone.
// Used by every Apply that can change a grant; hostkey/ratelimit/key/
// pricing writes don't affect grants and call c.snap.Store directly.
func (c *Catalog) commitWithGrants(s *Snapshot) {
	s.rebuildPolicyAllowSets()
	c.snap.Store(s)
}

// recoverAbsentLocked degrades an upsert for an id the snapshot doesn't hold
// to a full rebuild from the stores: an insert is indistinguishable from a
// re-appearance whose earlier delete-cascade stripped dependents (policy
// grants, relay keys, host keys) that only source truth still records, so an
// incremental patch would leave them permanently lost. Leaf kinds with no
// dependents (pricing, key) skip this and stay incremental. Caller must
// hold c.rmu; returns (handled, err).
func (c *Catalog) recoverAbsentLocked(kind refKind, id string) (bool, error) {
	if rowPresent(c.snap.Load(), refKey{Kind: kind, ID: id}) {
		return false, nil
	}
	// Apply* carries no ctx; this is a rare control-plane recovery path.
	return true, c.reloadLocked(context.Background())
}

// ── Reverse-join rebuild helpers ──────────────────────────────────────────

// rebuildModelsByPolicy recomputes the modelsByPolicy map from the current
// state of policiesByID and modelsByID.
func rebuildModelsByPolicy(s *Snapshot) {
	for polID, pol := range s.policiesByID {
		sl := s.modelsByPolicy[polID][:0]
		for _, id := range pol.Spec.ModelIDs {
			if m, ok := s.modelsByID[id]; ok {
				sl = append(sl, m)
			}
		}
		s.modelsByPolicy[polID] = sl
	}
}

func rebuildHostKeysByPolicy(s *Snapshot) {
	for polID, pol := range s.policiesByID {
		sl := s.hostKeysByPolicy[polID][:0]
		for _, id := range pol.Spec.HostKeyIDs {
			if k, ok := s.hostKeysByID[id]; ok {
				sl = append(sl, k)
			}
		}
		s.hostKeysByPolicy[polID] = sl
	}
}

// ── Slice helpers ─────────────────────────────────────────────────────────

func removeModelFromSlice(sl []*model.Model, id string) []*model.Model {
	out := sl[:0]
	for _, m := range sl {
		if m.Meta.ID != id {
			out = append(out, m)
		}
	}
	return out
}

func removeHostKeyFromSlice(sl []*hostkey.HostKey, id string) []*hostkey.HostKey {
	out := sl[:0]
	for _, k := range sl {
		if k.Meta.ID != id {
			out = append(out, k)
		}
	}
	return out
}
