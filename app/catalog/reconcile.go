package catalog

import (
	"context"

	"github.com/wyolet/relay/app/model"
)

// recoverAbsentLocked degrades an upsert for an id the snapshot doesn't hold
// to a full rebuild from the stores: an insert is indistinguishable from a
// re-appearance whose earlier delete-cascade stripped dependents (relay
// keys, host keys, bindings) that only source truth still records, so an
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
