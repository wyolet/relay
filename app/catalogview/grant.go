package catalogview

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/modelref"
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

// credentialLink is a host key a policy reaches through a pool; readOnly when
// it comes through a shared account, so it is never edited or revealed.
type credentialLink struct {
	id       string
	readOnly bool
}

// policyCredentials lists the host keys the policy reaches through its pools
// in pool order (each pool's accounts' keys, then its listed keys), each once
// at its first place, as PoolKeys lists them. Keys of an account closed by its
// owner's personal switch, and of a broken share, are left out, as the plan
// leaves them out.
func (idx *index) policyCredentials(p *policy.Policy) []credentialLink {
	seen := map[string]bool{}
	var out []credentialLink
	add := func(id string, readOnly bool) {
		if !seen[id] {
			seen[id] = true
			out = append(out, credentialLink{id: id, readOnly: readOnly})
		}
	}
	for _, pid := range p.Spec.PoolIDs {
		pl, ok := idx.poolByID[pid]
		if !ok {
			continue
		}
		for _, aid := range pl.Spec.AccountIDs {
			if a, ok := idx.accountByID[aid]; ok && idx.accountServes(a) {
				for _, k := range idx.keysByAccount[keyHolder(a)] {
					add(k.Meta.ID, a.IsShared())
				}
			}
		}
		for _, id := range pl.Spec.CredentialIDs {
			if k, ok := idx.hostkeyByID[id]; ok {
				if a, ok := idx.accountByID[k.Spec.AccountID]; ok && idx.ownerOff(a.Meta.Owner) {
					continue
				}
			}
			add(id, false)
		}
	}
	return out
}

// accountServes reports whether a pool listing a reaches its keys: a shared
// account only while its real account is a provider account at the same host,
// and either only while the real account's owner has personal accounts on.
func (idx *index) accountServes(a *account.Account) bool {
	if a.IsShared() {
		src, ok := idx.accountByID[a.Spec.SourceAccountID]
		if !ok || !src.IsProvider() || src.IsShared() || src.Spec.HostID != a.Spec.HostID {
			return false
		}
		a = src
	}
	return !idx.ownerOff(a.Meta.Owner)
}

// ownerOff reports whether o is a user whose personal accounts are turned off.
func (idx *index) ownerOff(o meta.Owner) bool {
	return idx.personalOff != nil && o.Kind == meta.OwnerUser && o.ID != "" && idx.personalOff(o.ID)
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
