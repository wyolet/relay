package catalog

import (
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/slug"
)

// snapshotRef links a Snapshot back to its owning Model. Stored in the
// snapshot-name index so request-time lookup can return both in one shot.
// HostID is set only for host-pinned aliases (the "@host" forms) — it tells
// resolution to bind that specific host; "" means no pin (caller didn't pin a
// host, so binding selection runs as normal).
type snapshotRef struct {
	Model    *model.Model
	Snapshot *model.Snapshot
	HostID   string
}

// hostPinSkip names hosts whose models carry non-normalizable upstream names
// (Bedrock ARNs, Vertex publisher paths with colons/slashes) that slug.From
// would mangle. We skip generating host-pinned aliases for them until per-host
// upstream-name handling lands; bare + provider-qualified addressing still
// works for their models.
// TODO(bedrock/vertex): drop this skip once host-specific upstream names are
// handled in alias generation.
var hostPinSkip = map[string]struct{}{
	"amazon-bedrock": {},
	"google-vertex":  {},
}

// indexModelSnapshots materializes every addressable alias for a model's
// snapshots into the bare-name + alias indices. Providers and hosts must
// already be indexed (build order guarantees this; reconcile clones a full
// snapshot).
func (s *Snapshot) indexModelSnapshots(m *model.Model) {
	provSlug, _ := s.ProviderSlug(m.Meta.Owner.ID)
	for i := range m.Spec.Snapshots {
		snap := &m.Spec.Snapshots[i]
		base := snapshotRef{Model: m, Snapshot: snap}
		s.snapshotsByName[snap.Name] = base
		if provSlug != "" {
			s.snapshotAliases[slug.From(provSlug+"/"+snap.Name)] = base
		}
		for _, hb := range s.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			h, ok := s.hostsByID[hb.Spec.HostID]
			if !ok {
				continue
			}
			if _, skip := hostPinSkip[h.Meta.Name]; skip {
				continue
			}
			pinned := snapshotRef{Model: m, Snapshot: snap, HostID: hb.Spec.HostID}
			s.snapshotAliases[slug.From(snap.Name+"@"+h.Meta.Name)] = pinned
			if provSlug != "" {
				s.snapshotAliases[slug.From(provSlug+"/"+snap.Name+"@"+h.Meta.Name)] = pinned
			}
		}
	}
	s.indexModelAliases(m, provSlug)
}

// deindexModelSnapshots removes a model's snapshots from both indices. Bare
// names are deleted only when this model still owns the entry; aliases are
// swept by owning-model id so deletion is robust even if the model's hosts
// were already evicted (the alias keys can no longer be recomputed in that
// case).
func (s *Snapshot) deindexModelSnapshots(m *model.Model) {
	for _, snap := range m.Spec.Snapshots {
		if ref, ok := s.snapshotsByName[snap.Name]; ok && ref.Model.Meta.ID == m.Meta.ID {
			delete(s.snapshotsByName, snap.Name)
		}
	}
	for k, ref := range s.snapshotAliases {
		if ref.Model.Meta.ID == m.Meta.ID {
			delete(s.snapshotAliases, k)
		}
	}
	s.deindexModelAliases(m)
}

// ResolveSnapshot maps a slug-normalized model ref to its model + snapshot,
// plus an optional pinned HostID (set when the ref named a host via "@host").
// Bare snapshot names win over synthesized aliases. The caller normalizes the
// key with slug.From.
func (s *Snapshot) ResolveSnapshot(key string) (*model.Model, *model.Snapshot, string, bool) {
	if r, ok := s.snapshotsByName[key]; ok {
		return r.Model, r.Snapshot, r.HostID, true
	}
	if r, ok := s.snapshotAliases[key]; ok {
		return r.Model, r.Snapshot, r.HostID, true
	}
	return nil, nil, "", false
}
