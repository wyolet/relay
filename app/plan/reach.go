// What an account's lane may use, worked out by the caller fold and route fill the request path runs, so a preview cannot drift from routing. The fold reads the published plans, optionally with one rung's policy replaced by a draft or by a saved policy read as if bound there; nothing is written and no plan is replaced.

package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/engine"
)

// Override stands in for the policy bound to one account during a reach read. AccountID "" is the system scope, which no account holds: the policy then sits alone above a lane reaching only system-owned keys.
type Override struct {
	AccountID string
	Policy    *policy.Policy
}

// Reach is what one lane may use: Refused when the whole ladder refuses, else the models it reaches and the models at its hosts it may not use.
type Reach struct {
	// Account is the account read; nil for the system scope.
	Account  *account.Account
	Refused  *Refusal
	Models   []ModelReach
	Excluded []ModelReach
	// Dropped are the links of the policy read that bring the lane nothing.
	Dropped []DroppedLink
}

// ModelReach is one model and how the lane reaches it, or, with Refused set, why it does not.
type ModelReach struct {
	Model    *model.Model
	Provider *provider.Provider
	// Accounts are, for a saved policy read in the accounts bound to it, the bound accounts that reach the model (or, excluded, that may not use it).
	Accounts []*account.Account
	// Hosts serve it for the lane, in failover order; for an excluded model, the reached hosts that bind it.
	Hosts []*host.Host
	// Via is what brings it, hop by hop: the pool (nil for a key reached by ownership) and the account each key draws on.
	Via      []ReachVia
	Upstream []UpstreamCheck
	Refused  *Refusal
}

// ReachVia is one pool and account a model is reached through; either may be nil.
type ReachVia struct {
	Pool    *pool.Pool
	Account *account.Account
}

// UpstreamCheck is one provider account whose keys could serve the model, with its tier policy and, when it refuses the model, why.
type UpstreamCheck struct {
	Account *account.Account
	// SharedAs is the shared account the keys are reached through; nil for the real account's own.
	SharedAs *account.Account
	Policy   *policy.Policy
	Refused  *Refusal
}

// ReachOf reads what account accountID's lane may use in v, with over standing in for one rung's policy when set. A team's or project's own account is read as a lane directly under it. nil when the account is unknown or heads no ladder.
func ReachOf(v *engine.View, accountID string, over *Override) *Reach {
	var ro *rungOverride
	if over != nil {
		ro = &rungOverride{policy: overridePolicy(v, over.Policy)}
	}
	if accountID == "" {
		if ro == nil {
			return nil
		}
		cp := foldSystem(v, ro)
		r := walkReach(cp, nil)
		r.Dropped = droppedLinks(v, cp, ro.policy, meta.Owner{Kind: meta.OwnerSystem}, nil)
		return r
	}
	acc := engine.Get[account.Account](v, accountID)
	if acc == nil {
		return nil
	}
	if ro != nil {
		ro.subject = accountSubject(acc.Meta.ID)
		if !acc.IsProvider() {
			ro.unusable = unusableIDs(v, ro.policy.Policy, acc.Meta.Owner, func(engine.Kind, string, meta.Owner) {})
		}
	}
	cp := foldFor(v, acc, ro)
	if cp == nil {
		return nil
	}
	r := walkReach(cp, acc)
	if ro != nil {
		r.Dropped = droppedLinks(v, cp, ro.policy, acc.Meta.Owner, ro.unusable)
	}
	return r
}

// ScopeHolds reports whether scope outer holds inner in v: the system holds everything, a scope itself, a team its projects.
func ScopeHolds(v *engine.View, outer, inner meta.Owner) bool { return scopeHolds(v, outer, inner) }

// OwnerAccount returns the account a policy owned by owner is previewed in: a team's or project's own account, a user's personal account. ok is false when owner is another kind or its account is not made yet; the system scope answers nil, true, as it has no account.
func OwnerAccount(v *engine.View, owner meta.Owner) (acc *account.Account, ok bool) {
	var place string
	switch owner.Kind {
	case meta.OwnerSystem:
		return nil, true
	case meta.OwnerTeam, meta.OwnerProject:
		place = string(owner.Kind) + ":" + owner.ID
	case meta.OwnerUser:
		place = "personal:" + owner.ID
	default:
		return nil, false
	}
	if m := engine.Get[ManagedAccount](v, place); m != nil && m.Row != nil {
		return m.Row, true
	}
	return nil, false
}

// overridePolicy is p as the plan expands it: the published expansion of a saved policy, else a draft's model list parsed alone.
func overridePolicy(v *engine.View, p *policy.Policy) *ExpandedPolicy {
	if ep := engine.Get[ExpandedPolicy](v, p.Meta.ID); ep != nil && ep.Policy == p {
		return ep
	}
	ep := &ExpandedPolicy{Policy: p}
	if len(p.Spec.Models) > 0 {
		g := &Grant{}
		for _, e := range modelEntries(p.Spec.Models) {
			g.refs = append(g.refs, e.ref)
		}
		ep.Grant = g
	}
	return ep
}

// foldFor folds acc's ladder as callerPlan does, from r, with over standing in for one rung's policy; nil when acc heads no ladder.
func foldFor[R *engine.Txn | *engine.View](r R, acc *account.Account, over *rungOverride) *CallerPlan {
	switch acc.DerivedKind() {
	case account.KindPersonal, account.KindProvider:
		if off := personalAccountOff(r, acc.Meta.Owner); off != nil {
			return refusedPlan(acc, personalOffRefusal(off, accountSubject(acc.Meta.ID)))
		}
	}
	rd, ok := readLane(r, acc, over, func(engine.Issue) {})
	if !ok {
		return nil
	}
	cp := &CallerPlan{Lane: acc, budgetAt: -1, linkedAt: -1}
	if h := acc.Spec.Holder; h != nil {
		cp.Holder = h.Key()
	}
	foldCaller(cp, rd)
	return cp
}

// foldSystem folds a lane under over's policy alone, reaching by ownership only system-owned keys.
func foldSystem[R *engine.Txn | *engine.View](r R, over *rungOverride) *CallerPlan {
	rd := newCallerReads(nil)
	lane := &rd.rungs[rungLane]
	lane.rung.ScopeID = string(meta.OwnerSystem)
	applyOverride(r, lane, over, &rd.in)
	readReach(r, &rd, nil, []string{"system:"})
	cp := &CallerPlan{Lane: &account.Account{Meta: meta.Metadata{Owner: meta.Owner{Kind: meta.OwnerSystem}}}, budgetAt: -1, linkedAt: -1}
	foldCaller(cp, rd)
	return cp
}

// walkReach runs the route fill for every model bound at a host the lane's pools are at.
func walkReach(cp *CallerPlan, acc *account.Account) *Reach {
	out := &Reach{Account: acc, Refused: cp.Refused}
	if cp.Refused != nil || cp.Routes.idx == nil {
		return out
	}
	snap := cp.Routes.idx.Snap
	reached := cp.reachedHosts()
	for _, m := range snap.AllModels() {
		if !boundAt(cp.Routes.idx, m, reached) {
			continue
		}
		mr := cp.reachModel(m)
		if mr.Refused != nil {
			for _, b := range snap.BindingsForModel(m.Meta.ID) {
				if h, ok := snap.Host(b.Spec.HostID); ok && reached[h.Meta.ID] && !containsHost(mr.Hosts, h) {
					mr.Hosts = append(mr.Hosts, h)
				}
			}
			out.Excluded = append(out.Excluded, mr)
		} else {
			out.Models = append(out.Models, mr)
		}
	}
	return out
}

// reachedHosts is the set of hosts the lane's pools are at.
func (cp *CallerPlan) reachedHosts() map[string]bool {
	out := make(map[string]bool, len(cp.Pools))
	for _, p := range cp.Pools {
		out[p.Host.Meta.ID] = true
	}
	return out
}

// boundAt reports whether a binding of m, in any state, is at one of hosts.
func boundAt(idx *CatalogIndex, m *model.Model, hosts map[string]bool) bool {
	for _, b := range idx.Snap.BindingsForModel(m.Meta.ID) {
		if hosts[b.Spec.HostID] {
			return true
		}
	}
	return false
}

// reachModel is the route fill's answer for m in this lane, with what brings it and each upstream account's verdict.
func (cp *CallerPlan) reachModel(m *model.Model) ModelReach {
	snap := cp.Routes.idx.Snap
	mr := ModelReach{Model: m}
	if p, ok := snap.Provider(m.Meta.Owner.ID); ok {
		mr.Provider = p
	}
	route := cp.Routes.route([]*model.Model{m}, nil, "")
	mr.Refused = route.Refused
	for i := range route.Hops {
		hop := &route.Hops[i]
		if !containsHost(mr.Hosts, hop.Host) {
			mr.Hosts = append(mr.Hosts, hop.Host)
		}
		for _, s := range hop.Slots {
			v := ReachVia{Pool: hop.Pool.Pool, Account: drawsOnAccount(s)}
			if !containsVia(mr.Via, v) {
				mr.Via = append(mr.Via, v)
			}
		}
	}
	mr.Upstream = cp.upstreamChecks(m)
	return mr
}

// upstreamChecks asks each provider account whose keys sit in the lane's pools at a host serving m whether its tier lets m through, as the route fill does per key.
func (cp *CallerPlan) upstreamChecks(m *model.Model) []UpstreamCheck {
	snap := cp.Routes.idx.Snap
	prov, _ := snap.ProviderSlug(m.Meta.Owner.ID)
	var out []UpstreamCheck
	seen := map[[2]string]bool{}
	for _, b := range snap.BindingsForModel(m.Meta.ID) {
		h, ok := snap.Host(b.Spec.HostID)
		if !b.IsEnabled() || !ok || !servesRequests(h) {
			continue
		}
		_, priced := snap.PricingForBinding(b)
		for _, p := range cp.Pools {
			if p.Host.Meta.ID != h.Meta.ID {
				continue
			}
			for _, s := range p.Slots {
				k := [2]string{s.Upstream.SubjectID, s.Via.SubjectID}
				if s.Upstream.Account == nil || seen[k] {
					continue
				}
				seen[k] = true
				c := UpstreamCheck{Account: s.Upstream.Account, SharedAs: s.Via.Account}
				if s.Upstream.Policy != nil {
					c.Policy = s.Upstream.Policy.Policy
				}
				if _, r := s.refuses(prov, m, b, h.Meta.Name, priced); r != nil {
					c.Refused = r
				}
				out = append(out, c)
			}
		}
	}
	return out
}

func drawsOnAccount(s *KeySlot) *account.Account {
	if s.Via.Account != nil {
		return s.Via.Account
	}
	if s.Account != nil {
		return s.Account.Account
	}
	return nil
}

func containsHost(hs []*host.Host, h *host.Host) bool {
	for _, x := range hs {
		if x.Meta.ID == h.Meta.ID {
			return true
		}
	}
	return false
}

func containsVia(vs []ReachVia, v ReachVia) bool {
	for _, x := range vs {
		if x == v {
			return true
		}
	}
	return false
}
