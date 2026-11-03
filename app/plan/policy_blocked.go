package plan

import (
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/engine"
)

// policyBound is a policy binding's Watch key into the policy checks: its policy.
func policyBound(b *policybinding.PolicyBinding) []string { return []string{b.Spec.PolicyID} }

// checkBlockedEntries files model_ref_blocked on each model entry that a rung above the policy, or an upstream tier, keeps from the accounts bound to it. Each bound account's ladder is folded and routed as a request would be, so the issue says what requests get.
func checkBlockedEntries(tx *engine.Txn, p *policy.Policy, idx *CatalogIndex, entries []modelEntry) {
	var lanes []*CallerPlan
	for _, b := range engine.Members[policybinding.PolicyBinding](tx) {
		if !b.IsEnabled() {
			continue
		}
		for _, aid := range b.Spec.AccountIDs {
			acc := engine.Get[account.Account](tx, aid)
			if acc == nil {
				continue
			}
			// A ladder refusing as a whole carries its own refusal; it blocks no entry in particular.
			if cp := foldFor(tx, acc, nil); cp != nil && cp.Refused == nil && cp.Routes.idx != nil {
				lanes = append(lanes, cp)
			}
		}
	}
	if len(lanes) == 0 {
		return
	}
	for _, e := range entries {
		models := entryModels(idx, e.ref)
		blocked := 0
		var first *Refusal
		for _, cp := range lanes {
			if r := cp.blocks(p.Meta.ID, models); r != nil {
				blocked++
				if first == nil {
					first = r
				}
			}
		}
		if blocked == 0 {
			continue
		}
		msg := e.ref.Raw + " is " + refusalReason(tx, first) + "; requests for it are refused"
		if blocked < len(lanes) {
			msg += fmt.Sprintf(" for %d of the %d accounts bound to this policy", blocked, len(lanes))
		}
		tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueModelRefBlocked, Source: engine.Ref{Kind: KindPolicy, ID: p.Meta.ID}, Field: fmt.Sprintf("spec.models[%d]", e.i),
			Target: engine.Ref{Kind: KindPolicy, ID: first.PolicyID}, Message: msg})
	}
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
	own := -1
	for i := range cp.Rungs {
		if ep := cp.Rungs[i].Policy; ep != nil && ep.Policy.Meta.ID == policyID {
			own = i
		}
	}
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

func rungIndex(r limits.Rung) int {
	switch r {
	case limits.RungTeam:
		return rungTeam
	case limits.RungProject:
		return rungProject
	}
	return rungLane
}
