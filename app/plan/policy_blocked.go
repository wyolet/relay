package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
)

// policyBound is a policy binding's Watch key into the policy checks: its policy.
func policyBound(b *policybinding.PolicyBinding) []string { return []string{b.Spec.PolicyID} }

// checkBlockedEntries files model_ref_blocked on each model entry that a rung above the policy, or an upstream tier, keeps from the accounts bound to it. Each bound account's ladder is folded and routed as a request would be, so the issue says what requests get.
func checkBlockedEntries(tx *engine.Txn, p *policy.Policy, idx *CatalogIndex, entries []modelEntry) {
	lanes, total := boundLadders(tx)
	narrowed := slices.ContainsFunc(lanes, func(l boundLadder) bool { return l.plan.narrowsAbove(p.Meta.ID) })
	if total == 0 || !narrowed {
		return
	}
	for _, e := range entries {
		models := entryModels(idx, e.ref)
		blocked := 0
		var first *Refusal
		for _, l := range lanes {
			if r := l.plan.blocks(p.Meta.ID, models); r != nil {
				blocked += l.accounts
				if first == nil {
					first = r
				}
			}
		}
		if blocked == 0 {
			continue
		}
		msg := e.ref.Raw + " is " + refusalReason(tx, first) + "; requests for it are refused"
		if blocked < total {
			msg += fmt.Sprintf(" for %d of the %d accounts bound to this policy", blocked, total)
		}
		tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueModelRefBlocked, Source: engine.Ref{Kind: KindPolicy, ID: p.Meta.ID}, Field: fmt.Sprintf("spec.models[%d]", e.i),
			Target: engine.Ref{Kind: KindPolicy, ID: first.PolicyID}, Message: msg})
	}
}

// boundLadder is one folded ladder and how many bound accounts it stands for.
type boundLadder struct {
	plan     *CallerPlan
	accounts int
}

// foldBound reads a bound account's ladder; a variable so tests can count the reads.
var foldBound = boundPlan

// boundPlan is a bound account's folded ladder: the lane plan the caller fold published for it or, for a team's or project's own account, which is no lane, the same fold of the rungs above and including it.
func boundPlan(tx *engine.Txn, acc *account.Account, over *rungOverride) *CallerPlan {
	if cp := engine.Get[CallerPlan](tx, accountSubject(acc.Meta.ID)); cp != nil {
		return cp
	}
	return foldFor(tx, acc, over)
}

// boundLadders folds the ladder of each account the policy's enabled bindings name, once per distinct ladder, and returns them with the number of accounts whose ladder does not refuse as a whole (such a ladder carries its own refusal and blocks no entry in particular).
func boundLadders(tx *engine.Txn) (lanes []boundLadder, total int) {
	seen := map[ladderKey]int{} // key → index in lanes, -1 for a refusing ladder
	for _, b := range engine.Members[policybinding.PolicyBinding](tx) {
		if !b.IsEnabled() {
			continue
		}
		for _, aid := range b.Spec.AccountIDs {
			acc := engine.Get[account.Account](tx, aid)
			if acc == nil {
				continue
			}
			k := ladderKeyOf(tx, acc)
			i, ok := seen[k]
			if !ok {
				i = -1
				if cp := foldBound(tx, acc, nil); cp != nil && cp.Refused == nil && cp.Routes.idx != nil {
					i = len(lanes)
					lanes = append(lanes, boundLadder{plan: cp})
				}
				seen[k] = i
			}
			if i >= 0 {
				lanes[i].accounts++
				total++
			}
		}
	}
	return lanes, total
}

// ladderKey is what sets one account's fold apart from another's in the same scope: the scope fixes the rungs above, the pools they link and what the scope owns, so accounts with equal keys fold to the same ladder.
type ladderKey struct {
	kind   account.Kind
	owner  meta.Owner
	closed bool
	// holderOff is the holder disabled or gone; holder is set only for a service account, whose shares serve it alone.
	holderOff bool
	holder    string
	// own is set for an account whose reach is its own: a provider account, or a shared account used as a lane.
	own string
	// rung is the policy bound to the account itself, with its refusal and the links its scope may not use.
	rung     *ExpandedPolicy
	refused  string
	unusable string
}

func ladderKeyOf[R *engine.Txn | *engine.View](tx R, acc *account.Account) ladderKey {
	k := ladderKey{kind: acc.DerivedKind(), owner: acc.Meta.Owner, closed: acc.Spec.Closed}
	if acc.IsProvider() {
		k.own = acc.Meta.ID
	}
	if h := acc.Spec.Holder; h != nil {
		switch h.Kind {
		case account.HolderUser:
			u := engine.Get[user.User](tx, h.ID)
			k.holderOff = u == nil || u.Disabled
		case account.HolderServiceAccount:
			sa := engine.Get[serviceaccount.ServiceAccount](tx, h.ID)
			k.holderOff, k.holder = sa == nil || !sa.IsEnabled(), h.Key()
		}
	}
	if rp := engine.Get[RungPolicy](tx, accountSubject(acc.Meta.ID)); rp != nil {
		k.rung, k.unusable = rp.Rung.Policy, strings.Join(rp.unusable, ",")
		if rp.Refused != nil {
			k.refused = rp.Refused.Code + "/" + rp.Refused.PolicyID
		}
	}
	return k
}

// entryModels lists the enabled models entry r grants at some host binding them.
func entryModels(idx *CatalogIndex, r modelref.Ref) []*model.Model {
	var cands []*model.Model
	switch {
	case !r.ModelWildcard:
		cands = idx.Snap.ModelsByName(r.Model)
	case !r.ProviderWildcard:
		if p, ok := idx.Snap.ProviderByName(r.Provider); ok {
			cands = idx.Snap.ModelsByProvider(p.Meta.ID)
		}
	default:
		cands = idx.Snap.AllModels()
	}
	var out []*model.Model
	for _, m := range cands {
		if !m.IsEnabled() {
			continue
		}
		for _, b := range idx.Snap.BindingsForModel(m.Meta.ID) {
			if h, ok := idx.Snap.Host(b.Spec.HostID); ok && entryMatches(idx, r, m, h) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// blocks returns the refusal of a rung above policyID's, or of an upstream tier, that keeps every one of models the lane's pools reach from the lane; nil when one is routed, or none is kept by such a rung.
func (cp *CallerPlan) blocks(policyID string, models []*model.Model) *Refusal {
	if !cp.narrowsAbove(policyID) {
		return nil
	}
	own := cp.rungOf(policyID)
	reached := cp.reachedHosts()
	var blk *Refusal
	for _, m := range models {
		if !boundAt(cp.Routes.idx, m, reached) {
			continue
		}
		route := cp.Routes.route([]*model.Model{m}, nil, "")
		if len(route.Hops) > 0 {
			return nil
		}
		if r := route.Refused; blk == nil && r != nil && r.Code == CodeModelNotAllowed && r.PolicyID != "" && (r.Rung == limits.RungAccount || rungIndex(r.Rung) < own) {
			blk = r
		}
	}
	return blk
}

// rungOf is the index of the innermost rung policyID is bound on, -1 when none.
func (cp *CallerPlan) rungOf(policyID string) int {
	own := -1
	for i := range cp.Rungs {
		if ep := cp.Rungs[i].Policy; ep != nil && ep.Policy.Meta.ID == policyID {
			own = i
		}
	}
	return own
}

// narrowsAbove reports whether a caller rung above policyID's, or the tier of a provider or shared account the lane's keys draw on, lists models: only such a rung can block an entry of the policy.
func (cp *CallerPlan) narrowsAbove(policyID string) bool {
	for i := range cp.rungOf(policyID) {
		if cp.Allow.grants[i] != nil {
			return true
		}
	}
	for _, p := range cp.Pools {
		for _, s := range p.Slots {
			for _, r := range [2]*Rung{&s.Upstream, &s.Via} {
				if r.Policy != nil && r.Policy.Grant != nil {
					return true
				}
			}
		}
	}
	return false
}

func rungIndex(r limits.Rung) int {
	switch r {
	case limits.RungTeam:
		return rungTeam
	case limits.RungProject:
		return rungProject
	}
	return rungLane
}
