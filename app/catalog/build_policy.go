package catalog

import (
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/meta"
)

func (s *Snapshot) addPolicies(pols []*policy.Policy, projects idSet) {
	for _, p := range pols {
		clean, keep := sanitizePolicy(p, projects)
		if !keep {
			continue
		}
		if !clean.IsEnabled() {
			// Kept outside the enabled indices so its dependents answer
			// policy_disabled; outbound refs stay registered so losing its
			// project still evicts it.
			s.disabledPoliciesByID[clean.Meta.ID] = clean
			s.registerRefs(refKey{Kind: refPolicy, ID: clean.Meta.ID}, outboundPolicyRefs(clean))
			continue
		}
		s.policiesByID[clean.Meta.ID] = clean
		s.policiesByName[clean.Meta.Name] = clean
		s.registerRefs(refKey{Kind: refPolicy, ID: clean.Meta.ID}, outboundPolicyRefs(clean))
	}
}

// policyResolvable answers "does a row naming this policy id keep working".
// A disabled policy counts: the row survives and its requests answer
// policy_disabled; only an absent policy drops the row.
func (s *Snapshot) policyResolvable(id string) bool {
	_, ok := s.policyLookup(id)
	return ok
}

// policyLookup returns the policy behind an id whether or not it is enabled.
// A PolicyBinding naming a disabled policy resolves through it.
func (s *Snapshot) policyLookup(id string) (*policy.Policy, bool) {
	if p, ok := s.policiesByID[id]; ok {
		return p, true
	}
	p, ok := s.disabledPoliciesByID[id]
	return p, ok
}

// sanitizePolicy drops a project-owned policy whose project is missing. Its
// links are kept as stored: the plan builder reports a link to a missing
// row rather than the snapshot hiding it.
func sanitizePolicy(p *policy.Policy, projects idSet) (*policy.Policy, bool) {
	if p.Meta.Owner.Kind == meta.OwnerProject && !projects(p.Meta.Owner.ID) {
		return nil, false
	}
	clean := *p
	return &clean, true
}
