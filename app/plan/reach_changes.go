// Which accounts reach something else in one view of a set of rows than in another, so a writer can tell the accounts its rows change besides those it meant to. Each account is folded as a caller in both views and compared by what decides its reach (refusal, rung policies and grants, pools and their slots); where that differs, by the reach preview itself.

package plan

import (
	"slices"
	"strconv"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
)

// ReachChange is an account whose reach differs between two views. Narrowed: it lost a model at a host or a way to one, or is refused where it was not, or with another code. Otherwise it only gained.
type ReachChange struct {
	AccountID string
	Narrowed  bool
}

// ChangedReach lists, by account id, the accounts both views hold whose reach as callers differs between before and after, leaving out those mine reports as the writer's own (it is given each account's caller plan in either view). An account only one view holds is the writer's to make or delete and is not listed.
func ChangedReach(before, after *engine.View, mine func(*CallerPlan) bool) []ReachChange {
	var out []ReachChange
	for acc := range engine.All[account.Account](after) {
		prev := engine.Get[account.Account](before, acc.Meta.ID)
		if prev == nil {
			continue
		}
		was, is := foldFor(before, prev, nil), foldFor(after, acc, nil)
		if was == nil || is == nil || mine(was) || mine(is) || laneState(was) == laneState(is) {
			continue
		}
		if narrowed, changed := compareReach(ReachOf(before, acc.Meta.ID, nil), ReachOf(after, acc.Meta.ID, nil)); changed {
			out = append(out, ReachChange{AccountID: acc.Meta.ID, Narrowed: narrowed})
		}
	}
	slices.SortFunc(out, func(a, b ReachChange) int { return strings.Compare(a.AccountID, b.AccountID) })
	return out
}

// compareReach reports whether after differs from before and, if so, whether it lost anything before had.
func compareReach(before, after *Reach) (narrowed, changed bool) {
	was, is := reachEntries(before), reachEntries(after)
	wasRefused, isRefused := refusalCode(before), refusalCode(after)
	for e := range was {
		if !is[e] {
			narrowed = true
		}
	}
	if isRefused != "" && isRefused != wasRefused {
		narrowed = true
	}
	changed = narrowed || wasRefused != isRefused || len(is) != len(was)
	return narrowed, changed
}

func refusalCode(r *Reach) string {
	if r == nil || r.Refused == nil {
		return ""
	}
	return r.Refused.Code
}

// reachEntries is what r reaches: each model at each host, and each pool and account it comes through.
func reachEntries(r *Reach) map[string]bool {
	out := map[string]bool{}
	if r == nil || r.Refused != nil {
		return out
	}
	for _, m := range r.Models {
		for _, h := range m.Hosts {
			out[m.Model.Meta.ID+"@"+h.Meta.ID] = true
		}
		for _, v := range m.Via {
			e := m.Model.Meta.ID + "/"
			if v.Pool != nil {
				e += v.Pool.Meta.ID
			}
			e += "/"
			if v.Account != nil {
				e += v.Account.Meta.ID
			}
			out[e] = true
		}
	}
	return out
}

// laneState is what decides cp's reach, as one string: its refusal, each rung's account, policy and whether it counts a budget, each rung's grant, and each pool with its slots.
func laneState(cp *CallerPlan) string {
	var b strings.Builder
	if cp.Refused != nil {
		b.WriteString("refused:" + cp.Refused.Code)
	}
	for i := range cp.Rungs {
		r := &cp.Rungs[i]
		b.WriteString("|" + r.SubjectID + ":" + policyState(r.Policy))
	}
	for _, g := range cp.Allow.grants {
		b.WriteString("|grant:")
		if g == nil {
			b.WriteString("*")
			continue
		}
		for _, ref := range g.refs {
			b.WriteString(policy.CanonicalModelRef(ref) + ",")
		}
	}
	for _, p := range cp.Pools {
		b.WriteString("|pool:")
		if p.Pool != nil {
			b.WriteString(p.Pool.Meta.ID)
		}
		if p.Host != nil {
			b.WriteString("@" + p.Host.Meta.ID)
		}
		for _, s := range p.Slots {
			b.WriteString(";" + slotState(s))
		}
	}
	return b.String()
}

func policyState(ep *ExpandedPolicy) string {
	if ep == nil || ep.Policy == nil {
		return ""
	}
	return ep.Policy.Meta.ID + "/" + strconv.Itoa(len(ep.Budgets))
}

func slotState(s *KeySlot) string {
	parts := []string{s.OnlyFor, s.Via.SubjectID, policyState(s.Via.Policy), s.Upstream.SubjectID, policyState(s.Upstream.Policy)}
	if s.Credential != nil {
		parts = append(parts, s.Credential.Meta.ID)
	}
	if s.Account != nil && s.Account.Account != nil {
		parts = append(parts, s.Account.Account.Meta.ID)
	}
	if s.Refused != nil {
		parts = append(parts, s.Refused.Code)
	}
	return strings.Join(parts, ",")
}
