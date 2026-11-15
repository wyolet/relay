package limits

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

var (
	testLimit = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl1"}}
	perMinute = ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: 60, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyTokenBucket}
)

// A counter's ID decides which bucket it counts in and its Group which store call it rides, so both are pinned whole.
func TestCounterConstructors(t *testing.T) {
	b := &budget.Budget{Meta: meta.Metadata{ID: "b1"}, Spec: budget.Spec{AmountUSD: "10", Period: budget.PeriodMonth}}
	const rule = "r:rl1:token-bucket:60:requests"
	for _, tc := range []struct {
		name      string
		got, want Counter
	}{
		{"caller rule", CallerRule(RungTeam, testLimit, perMinute, "p1", "team:t1", "g"),
			Counter{Kind: KindRule, Rung: RungTeam, PolicyID: "p1", SubjectID: "team:t1", RateLimitID: "rl1", Rule: perMinute, ID: rule + ":p:p1:s:team:t1", Group: "g"}},
		{"upstream rule", UpstreamRule(RungAccount, testLimit, perMinute, "p2", "account:a1", "", "g"),
			Counter{Kind: KindRule, Rung: RungAccount, PolicyID: "p2", SubjectID: "account:a1", RateLimitID: "rl1", Rule: perMinute, ID: rule + ":o:account:a1", Group: "g"}},
		{"upstream rule scoped to a model", UpstreamRule(RungAccount, testLimit, perMinute, "p2", "account:a1", "m1", "g"),
			Counter{Kind: KindRule, Rung: RungAccount, PolicyID: "p2", SubjectID: "account:a1", ModelID: "m1", RateLimitID: "rl1", Rule: perMinute, ID: rule + ":o:account:a1:m:m1", Group: "g"}},
		{"system rule", SystemRule(testLimit, perMinute, "ip:10.0.0.1"),
			Counter{Kind: KindRule, Rung: RungSystem, SubjectID: "ip:10.0.0.1", RateLimitID: "rl1", Rule: perMinute, ID: rule + ":s:ip:10.0.0.1", Group: "system:ip:10.0.0.1"}},
		{"budget", BudgetCounter(RungProject, b, "p3", "project:x", "g"),
			Counter{Kind: KindBudget, Rung: RungProject, PolicyID: "p3", SubjectID: "project:x", Budget: b, ID: "b:b1:month:s:project:x", Group: "g"}},
		{"per lane", CallerRule(RungTeam, testLimit, perMinute, "p1", "team:t1", "g").ForLane("account:a"),
			Counter{Kind: KindRule, Rung: RungTeam, PolicyID: "p1", SubjectID: "team:t1", Lane: "account:a", RateLimitID: "rl1", Rule: perMinute, ID: rule + ":p:p1:s:team:t1:l:account:a", Group: "g"}},
	} {
		if diff := cmp.Diff(tc.want, tc.got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
	shared := CallerRule(RungTeam, testLimit, perMinute, "p1", "team:t1", "g")
	if shared.ForLane("account:a"); shared.Lane != "" || shared.ID != rule+":p:p1:s:team:t1" {
		t.Errorf("ForLane changed the shared counter: %+v", shared)
	}
}

// What is and is not part of a counter's identity: an edit that keeps the ID keeps the count.
func TestCounterIdentity(t *testing.T) {
	caller := func(r ratelimit.Rule, policy string) string {
		return CallerRule(RungLane, testLimit, r, policy, "account:a", "g").ID
	}
	upstream := func(policy string) string {
		return UpstreamRule(RungAccount, testLimit, perMinute, policy, "account:a", "", "g").ID
	}
	budgetID := func(amount string, mode budget.Mode, period budget.Period) string {
		b := &budget.Budget{Meta: meta.Metadata{ID: "b1"}, Spec: budget.Spec{AmountUSD: amount, Mode: mode, Period: period}}
		return BudgetCounter(RungTeam, b, "p", "team:t", "g").ID
	}
	edit := func(f func(*ratelimit.Rule)) ratelimit.Rule {
		r := perMinute
		f(&r)
		return r
	}
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"a rule's amount", caller(perMinute, "p"), caller(edit(func(r *ratelimit.Rule) { r.Amount = 5000 }), "p"), true},
		{"a rule's window", caller(perMinute, "p"), caller(edit(func(r *ratelimit.Rule) { r.Window = ratelimit.Window(time.Hour) }), "p"), false},
		{"a rule's meter", caller(perMinute, "p"), caller(edit(func(r *ratelimit.Rule) { r.Meter = ratelimit.MeterTokens }), "p"), false},
		{"a rule's strategy", caller(perMinute, "p"), caller(edit(func(r *ratelimit.Rule) { r.Strategy = ratelimit.StrategyFixedWindow }), "p"), false},
		{"a caller rule's policy", caller(perMinute, "p"), caller(perMinute, "q"), false},
		{"an upstream rule's policy", upstream("p"), upstream("q"), true},
		{"a budget's amount and mode", budgetID("10", budget.ModeHard, budget.PeriodMonth), budgetID("99", budget.ModeAlert, budget.PeriodMonth), true},
		{"a budget's period", budgetID("10", budget.ModeHard, budget.PeriodMonth), budgetID("10", budget.ModeHard, budget.PeriodDay), false},
	} {
		if (tc.a == tc.b) != tc.same {
			t.Errorf("%s: ids %q and %q, want same = %v", tc.name, tc.a, tc.b, tc.same)
		}
	}
}

func TestRungReadingAndRefused(t *testing.T) {
	for r, want := range map[Rung]bool{RungAccount: true, RungTeam: false, RungProject: false, RungLane: false, RungSystem: false} {
		if r.Upstream() != want {
			t.Errorf("%s.Upstream() = %v, want %v", r, !want, want)
		}
	}
	for _, tc := range []struct{ used, amount, want int64 }{{0, 10, 10}, {4, 10, 6}, {10, 10, 0}, {15, 10, 0}} {
		if got := (Reading{Used: tc.used, Amount: tc.amount}).Remaining(); got != tc.want {
			t.Errorf("Remaining of %d used of %d = %d, want %d", tc.used, tc.amount, got, tc.want)
		}
	}
	c := CallerRule(RungTeam, testLimit, perMinute, "p1", "team:t1", "g")
	if got, want := (&Refused{Counter: &c}).Error(), "limits: team policy p1 refused counter r:rl1:token-bucket:60:requests:p:p1:s:team:t1"; got != want {
		t.Errorf("Refused.Error() = %q, want %q", got, want)
	}
}
