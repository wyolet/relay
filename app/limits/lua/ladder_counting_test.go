package lua_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

const usd = int64(1_000_000_000)

// ladder is a team with two projects, a service account in each, and m1 and m2 served at one priced host through a system credential.
type ladder struct {
	w        *plantest.World
	host     *host.Host
	team     *team.Team
	p1, p2   *project.Project
	sa1, sa2 *serviceaccount.ServiceAccount
	k1, k2   *key.Key
}

func newLadder() *ladder {
	w := &plantest.World{}
	prov := w.Provider("prov")
	l := &ladder{w: w, host: w.Host("h", "http://h.invalid")}
	for _, name := range []string{"m1", "m2"} {
		m := w.Model(prov, name)
		w.Bind(m, l.host, adapters.OpenAI)
		w.Price(m, l.host, 1)
	}
	l.team = w.Team()
	l.p1, l.p2 = w.Project(l.team), w.Project(l.team)
	l.sa1, l.sa2 = w.ServiceAccount(l.p1), w.ServiceAccount(l.p2)
	l.k1, l.k2 = w.ServiceAccountKey(l.sa1, "sk-one"), w.ServiceAccountKey(l.sa2, "sk-two")
	return l
}

func (l *ladder) withSystemCredential() *ladder {
	l.w.Credential(l.host, nil, plantest.System, "sk-system")
	return l
}

// saAccount is the service account's own account, which a binding binds.
func (l *ladder) saAccount(s *serviceaccount.ServiceAccount) *account.Account {
	return l.w.ServiceAccountAccount(s)
}

// clock is a settable limiter clock.
type clock struct{ now time.Time }

func (c *clock) read() time.Time { return c.now }

func newLimiter(t *testing.T, c *clock) *lua.Limiter {
	t.Helper()
	m := kv.NewMem()
	t.Cleanup(func() { _ = m.Close() })
	return lua.New(pkgratelimit.New(m, slog.New(slog.NewTextHandler(io.Discard, nil)), c.read))
}

// counters is everything a request of k for model must fit, through the route's first slot.
func counters(t *testing.T, plans *plantest.Plans, k *key.Key, model string) []limits.Counter {
	t.Helper()
	r := plans.Caller(t, k).Routes.Get(model)
	if r.Refused != nil || len(r.Hops) == 0 {
		t.Fatalf("route %s refused: %v", model, r.Refused)
	}
	return r.Hops[0].Counters[0]
}

// spend admits one request and settles it at cost; it fails the test when the request is refused.
func spend(t *testing.T, lim limits.Limiter, cs []limits.Counter, cost int64) {
	t.Helper()
	res, err := lim.Admit(context.Background(), cs, limits.Usage{})
	if err != nil {
		t.Fatalf("admit refused: %v", err)
	}
	res.Settle(limits.Usage{CostNanos: cost})
}

func refusedBy(t *testing.T, lim limits.Limiter, cs []limits.Counter) *limits.Refused {
	t.Helper()
	res, err := lim.Admit(context.Background(), cs, limits.Usage{})
	if err == nil {
		res.Release()
		return nil
	}
	var r *limits.Refused
	if !errors.As(err, &r) {
		t.Fatalf("admit failed with %v, want a refusal", err)
	}
	return r
}

func admitted(t *testing.T, lim limits.Limiter, cs []limits.Counter) bool {
	t.Helper()
	res, err := lim.Admit(context.Background(), cs, limits.Usage{})
	if err != nil {
		return false
	}
	res.Settle(limits.Usage{})
	return true
}

// budgetLadder binds a policy holding one budget of amount, period and mode to the first service account.
func budgetLadder(amount string, period budget.Period, mode budget.Mode) (*ladder, *budget.Budget, *policy.Policy) {
	l := newLadder().withSystemCredential()
	b := l.w.Budget(amount)
	b.Spec.Period, b.Spec.Mode = period, mode
	pol := l.w.Policy("budgeted", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID}}})
	l.w.BindPolicy(pol, l.saAccount(l.sa1))
	return l, b, pol
}

// A hard budget admits while spent is below the cap and refuses once spent reaches it, naming the budget's rung and policy.
func TestBudget_HardRefusesOnceSpentReachesTheCap(t *testing.T) {
	l, _, pol := budgetLadder("1", budget.PeriodMonth, budget.ModeHard)
	cs := counters(t, l.w.Load(t), l.k1, "m1")
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	spend(t, lim, cs, 6*usd/10)
	spend(t, lim, cs, 4*usd/10) // spent 0.6 < 1: admitted, reaching the cap exactly
	r := refusedBy(t, lim, cs)
	if r == nil {
		t.Fatal("admitted with spent equal to the cap")
	}
	if r.Counter.Kind != limits.KindBudget || r.Counter.Rung != limits.RungLane || r.Counter.PolicyID != pol.Meta.ID {
		t.Fatalf("refused by %+v, want the principal rung's budget of policy %s", r.Counter, pol.Meta.ID)
	}
}

// Nothing is reserved against a budget: a request admitted while spent is below the cap is charged its whole cost, and requests in flight together overshoot the cap.
func TestBudget_InFlightRequestsMayOvershoot(t *testing.T) {
	l, _, _ := budgetLadder("1", budget.PeriodMonth, budget.ModeHard)
	cs := counters(t, l.w.Load(t), l.k1, "m1")
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	a, err := lim.Admit(context.Background(), cs, limits.Usage{})
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	b, err := lim.Admit(context.Background(), cs, limits.Usage{})
	if err != nil {
		t.Fatalf("a second request in flight was refused before anything was spent: %v", err)
	}
	a.Settle(limits.Usage{CostNanos: 8 * usd / 10})
	b.Settle(limits.Usage{CostNanos: 8 * usd / 10})
	if refusedBy(t, lim, cs) == nil {
		t.Fatal("admitted after the in-flight requests overshot the cap")
	}
}

// A hard budget's refusal carries a retry-after at the next period start, calendar and UTC; weeks start on Monday.
func TestBudget_RetryAfterIsTheNextPeriodStart(t *testing.T) {
	for _, tc := range []struct {
		period budget.Period
		now    time.Time
		want   time.Duration
	}{
		{budget.PeriodDay, time.Date(2026, 11, 4, 22, 30, 0, 0, time.UTC), 90 * time.Minute},
		{budget.PeriodWeek, time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC), 12 * time.Hour},                                 // Sunday → Monday
		{budget.PeriodWeek, time.Date(2026, 11, 2, 0, 30, 0, 0, time.UTC), 6*24*time.Hour + 23*time.Hour + 30*time.Minute}, // Monday → next Monday
		{budget.PeriodMonth, time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC), time.Hour},
		{budget.PeriodMonth, time.Date(2027, 2, 27, 0, 0, 0, 0, time.UTC), 48 * time.Hour},
	} {
		t.Run(string(tc.period)+" "+tc.now.Format(time.RFC3339), func(t *testing.T) {
			l, _, _ := budgetLadder("1", tc.period, budget.ModeHard)
			cs := counters(t, l.w.Load(t), l.k1, "m1")
			lim := newLimiter(t, &clock{now: tc.now})
			spend(t, lim, cs, 2*usd)
			r := refusedBy(t, lim, cs)
			if r == nil {
				t.Fatal("admitted over the cap")
			}
			if d := r.RetryAfter - tc.want; d < -time.Second || d > time.Second {
				t.Fatalf("retry-after = %v, want %v (the next %s start)", r.RetryAfter, tc.want, tc.period)
			}
		})
	}
}

// At each period end the spent count starts again from zero; unused room does not carry over, and the boundary is UTC whatever zone the clock reads in.
func TestBudget_PeriodEndStartsFromZero(t *testing.T) {
	utc5 := time.FixedZone("UTC+5", 5*3600)
	for _, tc := range []struct {
		name          string
		period        budget.Period
		spentAt, same time.Time // same: later, still inside the period
		next          time.Time
	}{
		{"day", budget.PeriodDay,
			time.Date(2026, 11, 3, 18, 30, 0, 0, time.UTC).In(utc5), // 23:30 local on the 3rd
			time.Date(2026, 11, 3, 19, 30, 0, 0, time.UTC).In(utc5), // 00:30 local on the 4th, still the 3rd in UTC
			time.Date(2026, 11, 4, 0, 0, 1, 0, time.UTC)},
		{"week", budget.PeriodWeek,
			time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),  // Wednesday
			time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC), // Sunday
			time.Date(2026, 11, 9, 0, 0, 1, 0, time.UTC)},  // Monday
		{"month", budget.PeriodMonth,
			time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC),
			time.Date(2026, 12, 1, 0, 0, 1, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _, _ := budgetLadder("1", tc.period, budget.ModeHard)
			cs := counters(t, l.w.Load(t), l.k1, "m1")
			c := &clock{now: tc.spentAt}
			lim := newLimiter(t, c)
			spend(t, lim, cs, 3*usd)
			c.now = tc.same
			if refusedBy(t, lim, cs) == nil {
				t.Fatal("the count reset inside the period")
			}
			c.now = tc.next
			if r := refusedBy(t, lim, cs); r != nil {
				t.Fatalf("still refused after the period ended: %+v", r.Counter)
			}
		})
	}
}

// An alert budget never refuses, however far past the cap; its first crossing in a period is reported.
func TestBudget_AlertNeverRefuses(t *testing.T) {
	l, _, _ := budgetLadder("1", budget.PeriodMonth, budget.ModeAlert)
	cs := counters(t, l.w.Load(t), l.k1, "m1")
	var crossings int
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}).OnBudgetCrossed(func(*limits.Counter) { crossings++ })
	for range 4 {
		spend(t, lim, cs, 2*usd)
	}
	if !admitted(t, lim, cs) {
		t.Fatal("an alert budget refused")
	}
	if crossings != 1 {
		t.Fatalf("crossings reported = %d, want 1 (the first crossing in the period)", crossings)
	}
}

// Raising the cap mid-period gives room without resetting the count; lowering it again refuses on the same count.
func TestBudget_ChangingTheCapKeepsTheCount(t *testing.T) {
	l, b, _ := budgetLadder("1", budget.PeriodMonth, budget.ModeHard)
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})
	spend(t, lim, counters(t, plans, l.k1, "m1"), 15*usd/10)

	raised := *b
	raised.Spec.AmountUSD = "2"
	plantest.Apply(t, plans, &raised)
	cs := counters(t, plans, l.k1, "m1")
	spend(t, lim, cs, 6*usd/10) // 1.5 < 2
	if refusedBy(t, lim, cs) == nil {
		t.Fatal("admitted at 2.1 spent under a 2 cap: raising the cap reset the count")
	}

	lowered := raised
	lowered.Spec.AmountUSD = "1"
	plantest.Apply(t, plans, &lowered)
	if refusedBy(t, lim, counters(t, plans, l.k1, "m1")) == nil {
		t.Fatal("admitted after lowering the cap below the spent count")
	}
}

// A budget is counted per bound subject: a policy reused by two service accounts gives each its own spend.
func TestBudget_PolicyReusedBySubjectsCountsEachApart(t *testing.T) {
	l := newLadder().withSystemCredential()
	b := l.w.Budget("1")
	pol := l.w.Policy("shared", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID}}})
	l.w.BindPolicy(pol, l.saAccount(l.sa1), l.saAccount(l.sa2))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	spend(t, lim, counters(t, plans, l.k1, "m1"), 2*usd)
	if refusedBy(t, lim, counters(t, plans, l.k1, "m1")) == nil {
		t.Fatal("the first account was admitted over its cap")
	}
	if !admitted(t, lim, counters(t, plans, l.k2, "m1")) {
		t.Fatal("the second account was refused on the first one's spend")
	}
}

// A budget on a team-bound policy is one spend the whole team shares.
func TestBudget_TeamBoundBudgetIsSharedByTheTeam(t *testing.T) {
	l := newLadder().withSystemCredential()
	b := l.w.Budget("1")
	pol := l.w.Policy("team", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: b.Meta.ID}}})
	l.w.BindPolicy(pol, l.w.TeamAccount(l.team))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	spend(t, lim, counters(t, plans, l.k1, "m1"), 2*usd)
	r := refusedBy(t, lim, counters(t, plans, l.k2, "m1"))
	if r == nil {
		t.Fatal("a service account in another project of the team was admitted over the team's budget")
	}
	if r.Counter.Rung != limits.RungTeam {
		t.Fatalf("refused on the %s rung, want team", r.Counter.Rung)
	}
}

// An account-level budget caps every caller reaching the account, whoever they are.
func TestBudget_AccountBudgetCapsEveryCaller(t *testing.T) {
	l := newLadder()
	acct := l.w.Account(l.host)
	l.w.Credential(l.host, acct, plantest.System, "sk-acct")
	tier := l.w.Policy("tier", policy.Spec{Budgets: []policy.BudgetLink{{BudgetID: l.w.Budget("1").Meta.ID}}})
	l.w.BindPolicy(tier, acct)
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	spend(t, lim, counters(t, plans, l.k1, "m1"), 2*usd)
	r := refusedBy(t, lim, counters(t, plans, l.k2, "m1"))
	if r == nil || r.Counter.Rung != limits.RungAccount || r.Counter.PolicyID != tier.Meta.ID {
		t.Fatalf("refusal = %+v, want the account budget of policy %s", r, tier.Meta.ID)
	}
}

func hourly(n int64) ratelimit.Rule {
	return ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: n, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow}
}

// A caller rate limit counts per (bound subject, policy, rule): a policy reused by two subjects gives each its own bucket.
func TestRateLimit_PolicyReusedBySubjectsCountsEachApart(t *testing.T) {
	l := newLadder().withSystemCredential()
	rl := l.w.RateLimit(hourly(1))
	pol := l.w.Policy("one-an-hour", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	l.w.BindPolicy(pol, l.saAccount(l.sa1), l.saAccount(l.sa2))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first request refused")
	}
	r := refusedBy(t, lim, counters(t, plans, l.k1, "m1"))
	if r == nil || r.Counter.Rung != limits.RungLane || r.Counter.PolicyID != pol.Meta.ID || r.RetryAfter <= 0 {
		t.Fatalf("second request: refusal %+v, want the principal rule of policy %s with a retry-after", r, pol.Meta.ID)
	}
	if !admitted(t, lim, counters(t, plans, l.k2, "m1")) {
		t.Fatal("the other subject shares the first one's bucket")
	}
}

// A rate limit on a team-bound policy is one bucket every key in the team shares.
func TestRateLimit_TeamBoundPolicySharesOneBucket(t *testing.T) {
	l := newLadder().withSystemCredential()
	rl := l.w.RateLimit(hourly(1))
	pol := l.w.Policy("team", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	l.w.BindPolicy(pol, l.w.TeamAccount(l.team))
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first request refused")
	}
	r := refusedBy(t, lim, counters(t, plans, l.k2, "m1"))
	if r == nil || r.Counter.Rung != limits.RungTeam {
		t.Fatalf("a key in another project of the team: refusal %+v, want the team bucket", r)
	}
}

// A rate limit linked with a models scope (on a tier policy) counts only requests for those models, one bucket per model.
func TestRateLimit_ModelScopedRuleCountsOnlyItsModels(t *testing.T) {
	l := newLadder()
	extra := l.w.Model(l.w.Providers[0], "m3")
	l.w.Bind(extra, l.host, adapters.OpenAI)
	l.w.Price(extra, l.host, 1)
	acct := l.w.Account(l.host)
	l.w.Credential(l.host, acct, plantest.System, "sk-acct")
	rl := l.w.RateLimit(hourly(1))
	tier := l.w.Policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID, Models: []string{"prov/m1", "prov/m2"}}}})
	l.w.BindPolicy(tier, acct)
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first m1 request refused")
	}
	r := refusedBy(t, lim, counters(t, plans, l.k2, "m1"))
	if r == nil || r.Counter.Rung != limits.RungAccount || r.Counter.PolicyID != tier.Meta.ID {
		t.Fatalf("second m1 request: refusal %+v, want the account's scoped rule", r)
	}
	if !admitted(t, lim, counters(t, plans, l.k1, "m2")) {
		t.Fatal("m2 shares m1's bucket")
	}
	for range 3 {
		if !admitted(t, lim, counters(t, plans, l.k1, "m3")) {
			t.Fatal("a model outside the scope was counted by the scoped rule")
		}
	}
}

// A refusal on one rung releases what the other rungs reserved: the caller's token bucket is not drained by requests the account refused.
func TestRateLimit_RefusalReleasesTheOtherRungs(t *testing.T) {
	l := newLadder()
	acct := l.w.Account(l.host)
	l.w.Credential(l.host, acct, plantest.System, "sk-acct")
	callerRL := l.w.RateLimit(plantest.Rule(ratelimit.MeterRequests, 2, time.Hour))
	l.w.BindPolicy(l.w.Policy("caller", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: callerRL.Meta.ID}}}), l.saAccount(l.sa1))
	tierRL := l.w.RateLimit(hourly(1))
	l.w.BindPolicy(l.w.Policy("tier", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: tierRL.Meta.ID, Models: []string{"prov/m1"}}}}), acct)
	plans := l.w.Load(t)
	lim := newLimiter(t, &clock{now: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)})

	if !admitted(t, lim, counters(t, plans, l.k1, "m1")) {
		t.Fatal("first m1 request refused")
	}
	if r := refusedBy(t, lim, counters(t, plans, l.k1, "m1")); r == nil || r.Counter.Rung != limits.RungAccount {
		t.Fatalf("second m1 request: refusal %+v, want the account rule", r)
	}
	if !admitted(t, lim, counters(t, plans, l.k1, "m2")) {
		t.Fatal("the account's refusal kept the caller's token: m2 refused by the caller bucket")
	}
}
