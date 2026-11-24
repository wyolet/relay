// Which accounts reach something else in one view of a set of rows than in another, so a writer can tell the accounts its rows change besides those it meant to. Each account is folded as a caller in both views and compared by what decides its reach (refusal, rung policies and grants, pools and their slots); where that differs, by the reach preview itself.

package plan

import (
	"context"
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

// ChangedReach lists, by account id, the accounts both views hold whose reach as callers differs between before and after, leaving out those mine reports as the writer's own (it is given each account's caller plan in either view). An account only one view holds is the writer's to make or delete and is not listed. It stops with ctx's error once ctx ends.
func ChangedReach(ctx context.Context, before, after *engine.View, mine func(*CallerPlan) bool) ([]ReachChange, error) {
	var out []ReachChange
	for acc := range engine.All[account.Account](after) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		prev := engine.Get[account.Account](before, acc.Meta.ID)
		if prev == nil {
			continue
		}
		was, is := callerOf(before, prev), callerOf(after, acc)
		if was == nil || is == nil || mine(was) || mine(is) || laneState(was) == laneState(is) {
			continue
		}
		if lost, gained := diffReach(ReachOf(before, acc.Meta.ID, nil), ReachOf(after, acc.Meta.ID, nil)); lost || gained {
			out = append(out, ReachChange{AccountID: acc.Meta.ID, Narrowed: lost})
		}
	}
	slices.SortFunc(out, func(a, b ReachChange) int { return strings.Compare(a.AccountID, b.AccountID) })
	return out, nil
}

// callerOf is acc's caller plan in v: the one v holds for a lane, else acc folded as a caller; nil when acc heads no ladder.
func callerOf(v *engine.View, acc *account.Account) *CallerPlan {
	if cp := LanePlan(v, acc.Meta.ID); cp != nil {
		return cp
	}
	return foldFor(v, acc, nil)
}

// LaneReachChange is one lane and how the (model, host) pairs it is served at changed between two views: Lost and Gained say whether it lost or gained any; Unreachable, that it was served at some pair before and at none after. Another pool or account carrying the same model at the same host is no change.
type LaneReachChange struct {
	AccountID                 string
	Lost, Gained, Unreachable bool
}

// LanesUnder counts the lanes after holds that under reports, and lists, by account id, those of them whose reach differs from before's. It stops with ctx's error once ctx ends.
func LanesUnder(ctx context.Context, before, after *engine.View, under func(*CallerPlan) bool) (lanes int, changed []LaneReachChange, err error) {
	for cp := range engine.All[CallerPlan](after) {
		if err := ctx.Err(); err != nil {
			return 0, nil, err
		}
		if cp.Lane == nil || !under(cp) {
			continue
		}
		lanes++
		id := cp.Lane.Meta.ID
		if prev := LanePlan(before, id); prev != nil && laneState(prev) == laneState(cp) {
			continue
		}
		was, is := servedPairs(ReachOf(before, id, nil)), servedPairs(ReachOf(after, id, nil))
		c := LaneReachChange{AccountID: id, Unreachable: len(was) > 0 && len(is) == 0}
		for p := range was {
			c.Lost = c.Lost || !is[p]
		}
		for p := range is {
			c.Gained = c.Gained || !was[p]
		}
		if c.Lost || c.Gained {
			changed = append(changed, c)
		}
	}
	slices.SortFunc(changed, func(a, b LaneReachChange) int { return strings.Compare(a.AccountID, b.AccountID) })
	return lanes, changed, nil
}

// servedPairs is each model r reaches at each host it is served at; none for a lane refused as a whole.
func servedPairs(r *Reach) map[string]bool {
	out := map[string]bool{}
	if r == nil || r.Refused != nil {
		return out
	}
	for _, m := range r.Models {
		for _, h := range m.Hosts {
			out[m.Model.Meta.ID+"@"+h.Meta.ID] = true
		}
	}
	return out
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
