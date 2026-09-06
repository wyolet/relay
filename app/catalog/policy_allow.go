// policy_allow.go precomputes, per policy, the concrete set of (model, host)
// combinations the policy grants — so request-time authorization is an O(1)
// membership test instead of a per-request ref-matching loop.
//
// Only EXPLICIT-grant policies (ModelIDs and/or Models set) get a set. An
// implicit-wildcard policy (both empty) grants everything reachable via its
// hostkeys; materializing that would be the whole catalog, so it gets no set
// and PolicyAllowsCombo returns true — the hostkey-coverage gate at resolution
// is the real authorization in that case. The same sets gate the host tier
// policy a hostkey is attached to (see routing's key selection).
//
// Built off the hot path: once in Build, and recomputed on any reconcile that
// can change a grant (provider/host/model/policy writes). It reads policies +
// models + hosts + providers, so it must run after those are indexed.
package catalog

import (
	"log/slog"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
)

// comboKey identifies one allowed (model, host) pair.
type comboKey struct{ ModelID, HostID string }

// PolicyAllowsCombo reports whether the policy grants (modelID, hostID).
//
// An id naming no enabled policy grants nothing. Answering "allowed" for one
// is the wrong default in the direction that matters: a host key whose tier
// policy was switched off or deleted would pass the tier gate for every
// model, and the upstream reservation would find no rules to meter it by
// (D79). A policy that IS enabled and carries no materialized set is the
// implicit wildcard and allows everything — implicit-wildcard customer
// policies handle deprecation separately at resolution, and tier policies
// allow all by design.
func (s *Snapshot) PolicyAllowsCombo(policyID, modelID, hostID string) bool {
	if _, live := s.policiesByID[policyID]; !live {
		return false
	}
	set, ok := s.allowedCombosByPolicy[policyID]
	if !ok {
		return true
	}
	_, allowed := set[comboKey{ModelID: modelID, HostID: hostID}]
	return allowed
}

// rebuildPolicyAllowSets recomputes every explicit policy's allowed-combo set
// from scratch. Behaviour-equivalent to routing's old per-request check:
// legacy ModelIDs grant a model on all its hosts (no deprecation hiding);
// Models refs match per (provider, model, host) with wildcard grants hiding
// deprecated models unless IncludeDeprecated.
func (s *Snapshot) rebuildPolicyAllowSets() {
	s.allowedCombosByPolicy = make(map[string]map[comboKey]struct{}, len(s.policiesByID))
	for polID, p := range s.policiesByID {
		if len(p.Spec.ModelIDs) == 0 && len(p.Spec.Models) == 0 {
			continue // implicit wildcard — no set; PolicyAllowsCombo returns true
		}
		legacy := make(map[string]struct{}, len(p.Spec.ModelIDs))
		for _, id := range p.Spec.ModelIDs {
			legacy[id] = struct{}{}
		}
		// Parse refs once per policy, not once per (model, host) candidate.
		refs := make([]modelref.Ref, 0, len(p.Spec.Models))
		for _, raw := range p.Spec.Models {
			r, err := modelref.Parse(raw)
			if err != nil {
				// The grant silently stops granting; only a log tells the
				// operator why the policy suddenly allows less.
				slog.Warn("catalog: policy model ref unparseable, grant ignored",
					"policy", p.Meta.Name, "ref", raw, "err", err)
				continue
			}
			refs = append(refs, r)
		}
		set := map[comboKey]struct{}{}
		for _, m := range s.modelsByID {
			if !m.IsEnabled() {
				continue
			}
			_, isLegacy := legacy[m.Meta.ID]
			provSlug, _ := s.ProviderSlug(m.Meta.Owner.ID)
			hideDeprecated := modelDeprecated(m) && !p.Spec.IncludeDeprecated
			for _, hb := range s.BindingsForModel(m.Meta.ID) {
				if !hb.IsEnabled() {
					continue
				}
				h, ok := s.hostsByID[hb.Spec.HostID]
				if !ok {
					continue
				}
				granted := isLegacy || refsGrant(refs, provSlug, m.Meta.Name, h.Meta.Name, hideDeprecated)
				if granted {
					set[comboKey{ModelID: m.Meta.ID, HostID: hb.Spec.HostID}] = struct{}{}
				}
			}
		}
		s.allowedCombosByPolicy[polID] = set
	}
}

// refsGrant reports whether any ref matches (provider, model, host). When
// hideDeprecated is set, wildcard-model matches are rejected — an explicit
// "provider/model" still grants a deprecated model, a bare "provider" doesn't.
func refsGrant(refs []modelref.Ref, providerSlug, modelSlug, hostSlug string, hideDeprecated bool) bool {
	for _, ref := range refs {
		if !ref.Matches(providerSlug, modelSlug, hostSlug) {
			continue
		}
		if hideDeprecated && ref.ModelWildcard {
			continue
		}
		return true
	}
	return false
}

// modelDeprecated mirrors routing.isDeprecated (kept local to avoid an
// import cycle): deprecated and sunset models are hidden from wildcard grants.
func modelDeprecated(m *model.Model) bool {
	if m.Spec.Deprecation == nil {
		return false
	}
	switch m.Spec.Deprecation.Status {
	case model.DeprecationDeprecated, model.DeprecationSunset:
		return true
	}
	return false
}
