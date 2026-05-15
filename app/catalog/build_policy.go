package catalog

import "github.com/wyolet/relay/app/policy"

func (s *Snapshot) addPolicies(pols []*policy.Policy, models, keys, rls idSet) {
	for _, p := range pols {
		clean := sanitizePolicy(p, models, keys, rls)
		s.policiesByID[clean.Meta.ID] = clean
		s.policiesByName[clean.Meta.Name] = clean
		// outbound refs use the original spec so the COW reconciler can
		// reattach this policy when a previously-missing dependency returns.
		s.registerRefs(refKey{Kind: refPolicy, ID: clean.Meta.ID}, outboundPolicyRefs(p))
	}
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
// Spec stays in PG; only the snapshot copy is filtered.
func sanitizePolicy(p *policy.Policy, models, keys, rls idSet) *policy.Policy {
	clean := *p
	clean.Spec = p.Spec
	clean.Spec.ModelIDs = filterIDs(p.Spec.ModelIDs, models)
	clean.Spec.HostKeyIDs = filterIDs(p.Spec.HostKeyIDs, keys)
	if p.Spec.RateLimitID != "" {
		if _, ok := rls[p.Spec.RateLimitID]; !ok {
			clean.Spec.RateLimitID = ""
		}
	}
	if len(p.Spec.RLBindings) > 0 {
		bs := make([]policy.RLBinding, 0, len(p.Spec.RLBindings))
		for _, b := range p.Spec.RLBindings {
			if _, ok := rls[b.RateLimitID]; !ok {
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
	return &clean
}
