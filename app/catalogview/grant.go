package catalogview

import (
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
)

// policyGrantsModel mirrors one rung of the plan's reach and model rules, so
// the view shows the policies that could serve this model:
//
//   - a host-tier policy (Owner.Kind=host) applies only to its own host, and
//     only when that host serves the model.
//   - a policy linking pools applies only on their hosts; one linking none
//     does not narrow reach.
//   - an empty Models list grants every model on a candidate host; otherwise
//     the Models DSL must match (provider, model).
//
// Returns the host slug that satisfied the grant (for per-model limits) and,
// when the grant fails, a human-readable reason (for the ?debug view).
func (idx *index) policyGrantsModel(p *policy.Policy, m *model.Model, provSlug string, modelHosts map[string]string) (string, bool, string) {
	candidates := map[string]string{}
	for hostID, slug := range modelHosts {
		if idx.policyCoversHost(p, hostID) {
			candidates[hostID] = slug
		}
	}
	if len(candidates) == 0 {
		if p.Meta.Owner.Kind == meta.OwnerHost {
			return "", false, "host-tier policy's host has no binding for this model"
		}
		return "", false, "policy's pools reach no host that binds this model"
	}
	for _, slug := range candidates {
		if len(p.Spec.Models) == 0 || modelref.MatchAny(p.Spec.Models, provSlug, m.Meta.Name, slug) {
			return slug, true, ""
		}
	}
	return "", false, "policy's Models grant matches this model on no reachable host"
}

// policyCoversHost reports whether the policy may serve on a host: a
// host-tier policy only on its own host; a policy linking pools on their
// hosts; any other policy everywhere.
func (idx *index) policyCoversHost(p *policy.Policy, hostID string) bool {
	if p.Meta.Owner.Kind == meta.OwnerHost {
		return p.Meta.Owner.ID == hostID
	}
	if len(p.Spec.PoolIDs) == 0 {
		return true
	}
	_, ok := idx.policyReachHosts(p)[hostID]
	return ok
}

// policyReachHosts is the set of host ids the policy's linked pools
// authenticate to.
func (idx *index) policyReachHosts(p *policy.Policy) map[string]struct{} {
	out := make(map[string]struct{}, len(p.Spec.PoolIDs))
	for _, id := range p.Spec.PoolIDs {
		if pl, ok := idx.poolByID[id]; ok {
			out[pl.Spec.HostID] = struct{}{}
		}
	}
	return out
}

// policyCredentials lists the host keys the policy links through its pools,
// in order, each once.
func (idx *index) policyCredentials(p *policy.Policy) []string {
	seen := map[string]bool{}
	var out []string
	for _, pid := range p.Spec.PoolIDs {
		if pl, ok := idx.poolByID[pid]; ok {
			for _, id := range pl.Spec.CredentialIDs {
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// limitsFor returns the rules of every rate limit the policy links that
// applies to (provider, model, host): its unscoped links and the scoped ones
// whose models match.
func (idx *index) limitsFor(p *policy.Policy, provSlug, modelSlug, hostSlug string) []Limit {
	out := []Limit{}
	for _, l := range p.Spec.RateLimits {
		if len(l.Models) > 0 && !modelref.MatchAny(l.Models, provSlug, modelSlug, hostSlug) {
			continue
		}
		out = append(out, idx.limitsOf(l.RateLimitID)...)
	}
	return out
}
