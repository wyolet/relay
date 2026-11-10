package lua_test

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
)

var noon = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// A zero hard budget refuses all priced traffic.
func TestBudget_ZeroHardBudgetRefusesEverything(t *testing.T) {
	l, _, _ := budgetLadder("0", budget.PeriodMonth, budget.ModeHard)
	cs := counters(t, l.w.Load(t), l.k1, "m1")
	if refusedBy(t, newLimiter(t, &clock{now: noon}), cs) == nil {
		t.Fatal("a zero budget admitted a request")
	}
}

// Tier limits count per account: two credentials of one account, reached by callers of two different teams through their own pools, share one bucket; adding a key adds no capacity.
func TestRateLimit_AccountCountersAreSharedAcrossKeysAndTenants(t *testing.T) {
	l := newLadder()
	acct := l.w.Account(l.host)
	other := l.w.Team()
	otherSA := l.w.ServiceAccount(l.w.Project(other))
	otherKey := l.w.ServiceAccountKey(otherSA, "sk-other-team")
	a := l.w.Credential(l.host, acct, plantest.System, "sk-a")
	b := l.w.Credential(l.host, acct, plantest.System, "sk-b")
	l.w.BindPolicy(l.w.Policy("uses-a", policy.Spec{PoolIDs: []string{l.w.Pool(l.host, a).Meta.ID}}), l.saAccount(l.sa1))
	l.w.BindPolicy(l.w.Policy("uses-b", policy.Spec{PoolIDs: []string{l.w.Pool(l.host, b).Meta.ID}}), l.saAccount(otherSA))
	rl := l.w.RateLimit(hourly(1))
	l.w.BindPolicy(l.w.Policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), acct)
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: noon})

	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first request refused")
	}
	if refusedBy(t, lim, counters(t, plans, otherKey, "m1")) == nil {
		t.Fatal("another tenant's key of the same account was admitted past the account's limit")
	}
}

// Rotating a credential's value moves no counter: account counters are keyed by ids.
func TestRateLimit_CredentialRotationKeepsTheCount(t *testing.T) {
	l := newLadder()
	acct := l.w.Account(l.host)
	cred := l.w.Credential(l.host, acct, plantest.System, "sk-before")
	rl := l.w.RateLimit(hourly(1))
	l.w.BindPolicy(l.w.Policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), acct)
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: noon})
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first request refused")
	}

	rotated := *cred
	plantest.SetValue(&rotated, "sk-after")
	plantest.Apply(t, plans, &rotated)
	if refusedBy(t, lim, counters(t, plans, l.k1, "m1")) == nil {
		t.Fatal("a rotated credential started a fresh count")
	}
}

// Raising a rule's amount keeps its count; changing its window starts a fresh one.
func TestRateLimit_AmountEditKeepsCountWindowEditStartsFresh(t *testing.T) {
	l := newLadder().withSystemCredential()
	rl := l.w.RateLimit(hourly(1))
	l.w.BindPolicy(l.w.Policy("limited", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}}), l.saAccount(l.sa1))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: noon})
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first request refused")
	}

	raised := *rl
	raised.Spec.Rules = []ratelimit.Rule{hourly(2)}
	plantest.Apply(t, plans, &raised)
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("raising the amount to 2 left no room for a second request")
	}
	if refusedBy(t, lim, counters(t, plans, l.k1, "m1")) == nil {
		t.Fatal("raising the amount reset the count: a third request was admitted under 2 an hour")
	}

	rewindowed := raised
	rule := hourly(2)
	rule.Window = rule.Window * 2
	rewindowed.Spec.Rules = []ratelimit.Rule{rule}
	plantest.Apply(t, plans, &rewindowed)
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("a window edit kept the old count")
	}
}

// Re-pointing a subject at another policy starts fresh counts, even when both policies link the same rate limit.
func TestRateLimit_RebindingToAnotherPolicyStartsFresh(t *testing.T) {
	l := newLadder().withSystemCredential()
	rl := l.w.RateLimit(hourly(1))
	link := []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}
	first := l.w.Policy("first", policy.Spec{RateLimits: link})
	second := l.w.Policy("second", policy.Spec{RateLimits: link})
	pb := l.w.BindPolicy(first, l.saAccount(l.sa1))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: noon})
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first request refused")
	}

	moved := *pb
	moved.Spec.PolicyID = second.Meta.ID
	plantest.Apply(t, plans, &moved)
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("the subject kept the old policy's count after being re-pointed")
	}
}

// A policy bound to two teams gives each team its own spend: a second team never draws on the first one's.
func TestBudget_TeamBudgetIsNotSharedWithAnotherTeam(t *testing.T) {
	l := newLadder().withSystemCredential()
	b := l.w.Budget("1")
	pol := l.w.Policy("per-team", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID}}})
	other := l.w.Team()
	otherKey := l.w.ServiceAccountKey(l.w.ServiceAccount(l.w.Project(other)), "sk-other")
	l.w.BindPolicy(pol, l.w.TeamAccount(l.team), l.w.TeamAccount(other))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: noon})

	spend(t, lim, counters(t, plans, l.k1, "m1"), 2*usd)
	if !admitted(t, lim, counters(t, plans, otherKey, "m1")) {
		t.Fatal("a second team bound to the same policy drew on the first team's spend")
	}
}

// A rate limit linked with shared off on a team account counts per lane: two lanes of one team each get the full amount, while a shared link on the same account counts them together.
func TestRateLimit_PerLaneLinkCountsEachLaneApart(t *testing.T) {
	l := newLadder().withSystemCredential()
	rl := l.w.RateLimit(hourly(1))
	each := false
	l.w.BindPolicy(l.w.Policy("team", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID, Shared: &each}}}), l.w.TeamAccount(l.team))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: noon})
	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first lane's first request refused")
	}
	if !admitted(t, lim, counters(t, plans, l.k2, "m1")) {
		t.Fatal("a per-lane limit counted the second lane against the first")
	}
	if refusedBy(t, lim, counters(t, plans, l.k1, "m1")) == nil {
		t.Fatal("the first lane went past its own limit")
	}
}
