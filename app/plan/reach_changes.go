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
		if lost, gained := diffReach(ReachOf(before, acc.Meta.ID, nil), ReachOf(after, acc.Meta.ID, nil)); lost || gained {
			out = append(out, ReachChange{AccountID: acc.Meta.ID, Narrowed: lost})
		}
	}
	slices.SortFunc(out, func(a, b ReachChange) int { return strings.Compare(a.AccountID, b.AccountID) })
	return out
}

// LaneReachChange is one lane and how its reach changed between two views: it lost something, gained something, or, Unreachable, served a model at some host before and serves none after.
type LaneReachChange struct {
	AccountID                      string
	Narrowed, Widened, Unreachable bool
}

// LanesUnder counts the lanes after holds that under reports, and lists, by account id, those of them whose reach differs from before's.
func LanesUnder(before, after *engine.View, under func(*CallerPlan) bool) (lanes int, changed []LaneReachChange) {
	for cp := range engine.All[CallerPlan](after) {
		if cp.Lane == nil || !under(cp) {
			continue
		}
		lanes++
		id := cp.Lane.Meta.ID
		if prev := LanePlan(before, id); prev != nil && laneState(prev) == laneState(cp) {
			continue
		}
		was, is := ReachOf(before, id, nil), ReachOf(after, id, nil)
		if lost, gained := diffReach(was, is); lost || gained {
			changed = append(changed, LaneReachChange{AccountID: id, Narrowed: lost, Widened: gained, Unreachable: serves(was) && !serves(is)})
		}
	}
	slices.SortFunc(changed, func(a, b LaneReachChange) int { return strings.Compare(a.AccountID, b.AccountID) })
	return lanes, changed
}

// serves reports whether r reaches some model at some host.
func serves(r *Reach) bool {
	if r == nil || r.Refused != nil {
		return false
	}
	for _, m := range r.Models {
		if len(m.Hosts) > 0 {
			return true
		}
	}
	return false
}

// diffReach reports whether after lost anything before had (a refusal it did not have, or with another code, counts) and whether it gained anything (a refusal lifted counts).
func diffReach(before, after *Reach) (lost, gained bool) {
	was, is := reachEntries(before), reachEntries(after)
	wasRefused, isRefused := refusalCode(before), refusalCode(after)
	for e := range was {
		if !is[e] {
			lost = true
		}
	}
	for e := range is {
		if !was[e] {
			gained = true
		}
	}
	if isRefused != "" && isRefused != wasRefused {
		lost = true
	}
	if wasRefused != "" && isRefused == "" {
		gained = true
	}
	return lost, gained
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
