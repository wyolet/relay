package catalog

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

func (s *Snapshot) addPolicies(pols []*policy.Policy, models, keys, rls, projects idSet) {
	for _, p := range pols {
		clean, keep := sanitizePolicy(p, models, keys, rls, projects)
		if !keep {
			continue
		}
		if !clean.IsEnabled() {
			// Out of every routing index, but remembered so the rows that
			// name it survive and resolution can answer policy_disabled. Its
			// outbound refs are still registered: losing its project must
			// evict it, or its dependents keep answering 403 forever.
			s.disabledPoliciesByID[clean.Meta.ID] = clean
			s.registerRefs(refKey{Kind: refPolicy, ID: clean.Meta.ID}, outboundPolicyRefs(clean))
			continue
		}
		s.policiesByID[clean.Meta.ID] = clean
		s.policiesByName[clean.Meta.Name] = clean
		// Refs mirror the stored (sanitized) row so incremental delete/replace
		// unregisters exactly what was registered. Re-appearance of a missing
		// dependency is handled by the absent-id full reload in reconcile, not
		// by ref-web reattachment.
		s.registerRefs(refKey{Kind: refPolicy, ID: clean.Meta.ID}, outboundPolicyRefs(clean))
	}
}

// policyResolvable answers "does a row naming this policy id keep working".
// A disabled policy counts: the row survives and its requests answer
// policy_disabled (D77); only an absent policy drops the row.
func (s *Snapshot) policyResolvable(id string) bool {
	_, ok := s.policyLookup(id)
	return ok
}

// policyLookup returns the policy behind an id whether or not it is enabled.
// The rows that survive a disable resolve through it: the Key, ServiceAccount
// and PolicyBinding of D77, and a host key's tier policy, which comes back
// intact when the tier is switched on again.
func (s *Snapshot) policyLookup(id string) (*policy.Policy, bool) {
	if p, ok := s.policiesByID[id]; ok {
		return p, true
	}
	p, ok := s.disabledPoliciesByID[id]
	return p, ok
}

// computePolicyReverseJoins must run after policies, models, hostkeys, and
// ratelimits are all in the snapshot, since the joins read those maps.
func (s *Snapshot) computePolicyReverseJoins() {
	for _, p := range s.policiesByID {
		for _, id := range p.Spec.ModelIDs {
			if m, ok := s.modelsByID[id]; ok {
				s.modelsByPolicy[p.Meta.ID] = append(s.modelsByPolicy[p.Meta.ID], m)
			}
		}
		for _, id := range p.Spec.HostKeyIDs {
			if k, ok := s.hostKeysByID[id]; ok {
				s.hostKeysByPolicy[p.Meta.ID] = append(s.hostKeysByPolicy[p.Meta.ID], k)
			}
		}
		if p.Spec.RateLimitID != "" {
			if r, ok := s.rateLimitsByID[p.Spec.RateLimitID]; ok {
				s.rateLimitByPolicy[p.Meta.ID] = r
			}
		}
	}
}

// sanitizePolicy drops Spec refs (ModelIDs, HostKeyIDs, RateLimitID,
// RLBindings) whose targets aren't in the enabled-id sets. The original
// Spec stays in PG; only the snapshot copy is filtered. The owning
// Project is the one hard ref: without it the whole row is dropped.
func sanitizePolicy(p *policy.Policy, models, keys, rls, projects idSet) (*policy.Policy, bool) {
	if p.Meta.Owner.Kind == meta.OwnerProject {
		if !projects(p.Meta.Owner.ID) {
			return nil, false
		}
	}
	clean := *p
	clean.Spec = p.Spec
	clean.Spec.ModelIDs = filterIDs(p.Spec.ModelIDs, models)
	clean.Spec.HostKeyIDs = filterIDs(p.Spec.HostKeyIDs, keys)
	if p.Spec.RateLimitID != "" {
		if !rls(p.Spec.RateLimitID) {
			clean.Spec.RateLimitID = ""
		}
	}
	if len(p.Spec.RLBindings) > 0 {
		bs := make([]policy.RLBinding, 0, len(p.Spec.RLBindings))
		for _, b := range p.Spec.RLBindings {
			if !rls(b.RateLimitID) {
				continue
			}
			bs = append(bs, b)
		}
		if len(bs) == 0 {
			clean.Spec.RLBindings = nil
		} else {
			clean.Spec.RLBindings = bs
		}
	}
	return &clean, true
}
