package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/pkg/engine"
)

// RungLimits is one caller rung a lane climbs, with what its policy links and the counters admission counts each link in.
type RungLimits struct {
	Rung       *Rung
	RateLimits []RateLimitCounters
	Budgets    []BudgetCounter
}

// RateLimitCounters is one linked rate limit and its counters, one per rule in rule order. PerLane is set, and Counters nil, when the link counts per lane and the rungs were read for no lane.
type RateLimitCounters struct {
	Link     *LinkedRateLimit
	Counters []limits.Counter
	PerLane  bool
}

// BudgetCounter is one linked budget and its counter. PerLane is set, and Counter zero, as for RateLimitCounters.
type BudgetCounter struct {
	Link    *LinkedBudget
	Counter limits.Counter
	PerLane bool
}

// Limits returns the caller rungs the lane climbs that have an account, outermost first: a team's or project's account before the lane's own. A rung's links are listed whether or not its binding refuses; a provider account used as a lane climbs none.
func (cp *CallerPlan) Limits() []RungLimits {
	lane, group := cp.counterGroup()
	var out []RungLimits
	for i := range cp.Rungs {
		r := &cp.Rungs[i]
		if r.SubjectID == "" || r.Account == nil {
			continue
		}
		out = append(out, rungLimits(r, lane, group))
	}
	return out
}

// ScopeLimits returns the rungs of a team's or project's own account in v, outermost first: the team's account, then the project's for a project account. No lane climbs them here, so links counted per lane carry no counters. nil for any other account.
func ScopeLimits(v *engine.View, acc *account.Account) []RungLimits {
	var teamID string
	switch acc.DerivedKind() {
	case account.KindTeam:
		teamID = acc.Meta.Owner.ID
	case account.KindProject:
		if p := engine.Get[project.Project](v, acc.Meta.Owner.ID); p != nil {
			teamID = p.Spec.TeamID
		}
	default:
		return nil
	}
	var rungs []*Rung
	group := ""
	if m := engine.Get[ManagedAccount](v, "team:"+teamID); m != nil && m.Row != nil {
		rungs = append(rungs, viewRung(v, limits.RungTeam, m.Row))
		group = accountSubject(m.Row.Meta.ID)
	}
	if acc.DerivedKind() == account.KindProject {
		rungs = append(rungs, viewRung(v, limits.RungProject, acc))
	}
	out := make([]RungLimits, 0, len(rungs))
	for _, r := range rungs {
		out = append(out, rungLimits(r, "", group))
	}
	return out
}

// viewRung is a team's or project's account as a caller rung, with the policy bound to it in v.
func viewRung(v *engine.View, kind limits.Rung, a *account.Account) *Rung {
	r := &Rung{Kind: kind, SubjectID: accountSubject(a.Meta.ID), Account: a, ScopeID: a.Meta.Owner.ID}
	if rp := engine.Get[RungPolicy](v, r.SubjectID); rp != nil {
		r.Policy = rp.Rung.Policy
	}
	return r
}

// rungLimits lists r's links with their counters; with lane empty, a link counted per lane is marked rather than counted.
func rungLimits(r *Rung, lane, group string) RungLimits {
	rl := RungLimits{Rung: r}
	ep := r.Policy
	if ep == nil {
		return rl
	}
	for j := range ep.RateLimits {
		l := &ep.RateLimits[j]
		if !l.Shared && lane == "" {
			rl.RateLimits = append(rl.RateLimits, RateLimitCounters{Link: l, PerLane: true})
			continue
		}
		rl.RateLimits = append(rl.RateLimits, RateLimitCounters{Link: l, Counters: rateLimitCounters(r, l, lane, group)})
	}
	for j := range ep.Budgets {
		b := &ep.Budgets[j]
		if !b.Shared && lane == "" {
			rl.Budgets = append(rl.Budgets, BudgetCounter{Link: b, PerLane: true})
			continue
		}
		rl.Budgets = append(rl.Budgets, BudgetCounter{Link: b, Counter: budgetCounter(r, b, lane, group)})
	}
	return rl
}

// counterGroup is the lane's subject and the atomicity group of its caller counters: the team's, so the team's bucket and the lane's ride one store call, else the lane's own.
func (cp *CallerPlan) counterGroup() (lane, group string) {
	lane = cp.Rungs[rungLane].SubjectID
	group = lane
	if t := cp.Rungs[rungTeam].SubjectID; t != "" {
		group = t
	}
	return lane, group
}

// rateLimitCounters is a caller rung's counters for one linked rate limit: per (rule, policy, bound account) when shared, per lane under it otherwise.
func rateLimitCounters(r *Rung, l *LinkedRateLimit, lane, group string) []limits.Counter {
	pid := r.Policy.Policy.Meta.ID
	cs := make([]limits.Counter, 0, len(l.RateLimit.Spec.Rules))
	for _, rule := range l.RateLimit.Spec.Rules {
		c := limits.CallerRule(r.Kind, l.RateLimit, rule, pid, r.SubjectID, group)
		if !l.Shared {
			c = c.ForLane(lane)
		}
		cs = append(cs, c)
	}
	return cs
}

// budgetCounter is a caller rung's counter for one linked budget, shared or per lane as rateLimitCounters.
func budgetCounter(r *Rung, b *LinkedBudget, lane, group string) limits.Counter {
	c := limits.BudgetCounter(r.Kind, b.Budget, r.Policy.Policy.Meta.ID, r.SubjectID, group)
	if !b.Shared {
		c = c.ForLane(lane)
	}
	return c
}

// LanePlan returns the caller plan of the account with id accountID in v; nil for a team's or project's own account, which is never a lane, and for a lane whose holder or scope is gone.
func LanePlan(v *engine.View, accountID string) *CallerPlan {
	return engine.Get[CallerPlan](v, accountSubject(accountID))
}

// AccountPolicy returns the policy bound to the account with id accountID in v; nil when no binding names it, or when two do.
func AccountPolicy(v *engine.View, accountID string) *policy.Policy {
	rp := engine.Get[RungPolicy](v, accountSubject(accountID))
	if rp == nil || rp.Rung.Policy == nil {
		return nil
	}
	return rp.Rung.Policy.Policy
}

// KeyLane returns the id of the account k's requests run in by default in v: its accountId, or its principal's own account when it names none; "" when that account is not made yet.
func KeyLane(v *engine.View, k *key.Key) string { return keyLaneID(v, k) }

func keyLaneID[R *engine.Txn | *engine.View](r R, k *key.Key) string {
	if id := k.Spec.AccountID; id != "" {
		return id
	}
	if m := engine.Get[ManagedAccount](r, ownAccountKey(k.Spec.Principal)); m != nil && m.Row != nil {
		return m.Row.Meta.ID
	}
	return ""
}
