package catalog

import (
	"sort"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

// addBindings folds HostBindings into the snapshot. A binding is dropped if
// its Model or Host is missing/disabled; an optional Pricing ref that points
// at a missing pricing is cleared (the binding still serves, just unpriced).
//
// Indexes: bindingsByID, bindingsByModelHost (O(1) routing), and
// bindingsByModel (sorted, for per-model enumeration / alias generation).
func (s *Snapshot) addBindings(bindings []*binding.Binding, models, hosts idSet) {
	for _, b := range bindings {
		clean, keep := s.sanitizeBinding(b, models, hosts)
		if !keep {
			continue
		}
		s.bindingsByID[clean.Meta.ID] = clean
		s.bindingsByModelHost[clean.Spec.ModelID+"|"+clean.Spec.HostID] = clean
		s.bindingsByModel[clean.Spec.ModelID] = append(s.bindingsByModel[clean.Spec.ModelID], clean)
		s.registerRefs(refKey{Kind: refBinding, ID: clean.Meta.ID}, outboundBindingRefs(clean))
	}
	for modelID := range s.bindingsByModel {
		list := s.bindingsByModel[modelID]
		sort.Slice(list, func(i, j int) bool { return list[i].Meta.Name < list[j].Meta.Name })
	}
}

// syntheticBindings converts a model's embedded model.Spec.Hosts into binding
// values on the fly. It is the migration bridge: BindingsForModel returns these
// for any model that has no standalone HostBinding rows yet, so routing/aliases
// /allow-sets read bindings uniformly whether or not the catalog has been
// migrated to standalone HostBinding YAML. Computed at read time (not stored)
// so it stays correct under the incremental COW reconciler — no rebuild hook
// needed. Removed in the PR that drops model.Spec.Hosts entirely.
//
// Synthetic ids are deterministic (modelID|hostID). Host validity is checked by
// the caller paths (they look up the host and skip misses), mirroring how the
// embedded-hosts loop behaved.
func syntheticBindings(m *model.Model) []*binding.Binding {
	if len(m.Spec.Hosts) == 0 {
		return nil
	}
	out := make([]*binding.Binding, 0, len(m.Spec.Hosts))
	for i := range m.Spec.Hosts {
		hb := &m.Spec.Hosts[i]
		out = append(out, &binding.Binding{
			Meta: meta.Metadata{
				ID:    m.Meta.ID + "|" + hb.HostID,
				Name:  m.Meta.Name + "@" + hb.HostID,
				Owner: meta.Owner{Kind: meta.OwnerSystem},
			},
			Spec: binding.Spec{
				ModelID:   m.Meta.ID,
				HostID:    hb.HostID,
				Adapter:   hb.Adapter,
				Enabled:   hb.Enabled,
				Snapshots: hb.Snapshots,
			},
		})
	}
	return out
}

// sanitizeBinding drops the binding if its Model or Host is missing, and
// clears a dangling Pricing ref. Pricing presence is checked against the
// already-built pricingsByID (addPricings runs before addBindings).
func (s *Snapshot) sanitizeBinding(b *binding.Binding, models, hosts idSet) (*binding.Binding, bool) {
	if _, ok := models[b.Spec.ModelID]; !ok {
		return nil, false
	}
	if _, ok := hosts[b.Spec.HostID]; !ok {
		return nil, false
	}
	clean := *b
	clean.Spec = b.Spec
	if clean.Spec.PricingID != "" {
		if _, ok := s.pricingsByID[clean.Spec.PricingID]; !ok {
			clean.Spec.PricingID = ""
		}
	}
	return &clean, true
}
