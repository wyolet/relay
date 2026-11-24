package catalog

import (
	"github.com/wyolet/relay/pkg/catalogkind/model"
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
			pins := s.snapshotAliases
			if !hb.Serves(snap.Name) {
				pins = s.unservedPins
			}
			pinned := snapshotRef{Model: m, Snapshot: snap, HostID: hb.Spec.HostID}
			pins[slug.From(snap.Name+"@"+h.Meta.Name)] = pinned
			if provSlug != "" {
				pins[slug.From(provSlug+"/"+snap.Name+"@"+h.Meta.Name)] = pinned
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
	for _, pins := range [2]map[string]snapshotRef{s.snapshotAliases, s.unservedPins} {
		for k, ref := range pins {
			if ref.Model.Meta.ID == m.Meta.ID {
				delete(pins, k)
			}
		}
	}
	s.deindexModelAliases(m)
}

// reindexModelSnapshots recomputes a model's alias keys. Provider- and
// host-qualified keys embed the slug, so a provider/host rename must call it.
func (s *Snapshot) reindexModelSnapshots(m *model.Model) {
	s.deindexModelSnapshots(m)
	s.indexModelSnapshots(m)
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

// UnservedPin reports whether a slug-normalized key is a host-pinned snapshot name ("snapshot@host", optionally provider-qualified) whose host's enabled binding does not serve the snapshot, and returns that snapshot's model. Such a key resolves to nothing: it is never answered by a declared alias. The caller normalizes the key with slug.From.
func (s *Snapshot) UnservedPin(key string) (*model.Model, bool) {
	r, ok := s.unservedPins[key]
	return r.Model, ok
}
