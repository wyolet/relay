package limits

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

func TestCounterIDs_Golden(t *testing.T) {
	rl := &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl1"}}
	r := ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: 60, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyTokenBucket}
	b := &budget.Budget{Meta: meta.Metadata{ID: "b1"}, Spec: budget.Spec{AmountUSD: "10", Period: budget.PeriodMonth}}
	for _, tc := range []struct{ got, want string }{
		{CallerRule(RungTeam, rl, r, "p1", "team:t1", "team:t1").ID, "r:rl1:token-bucket:60:requests:p:p1:s:team:t1"},
		{UpstreamRule(RungAccount, rl, r, "p2", "account:a1", "", "account:a1").ID, "r:rl1:token-bucket:60:requests:o:account:a1"},
		{UpstreamRule(RungAccount, rl, r, "p2", "account:a1", "m1", "account:a1").ID, "r:rl1:token-bucket:60:requests:o:account:a1:m:m1"},
		{BudgetCounter(RungProject, b, "p3", "project:x", "team:t1").ID, "b:b1:month:s:project:x"},
	} {
		if tc.got != tc.want {
			t.Errorf("id = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestCounterIDs_AmountKeepsWindowChanges(t *testing.T) {
	rl := &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl1"}}
	base := ratelimit.Rule{Meter: ratelimit.MeterTokens, Amount: 1000, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategySlidingWindow}
	raised, wider := base, base
	raised.Amount = 5000
	wider.Window = ratelimit.Window(time.Hour)
	id := func(r ratelimit.Rule) string { return CallerRule(RungPrincipal, rl, r, "p", "user:u", "user:u").ID }
	if id(base) != id(raised) {
		t.Error("an amount edit changed the counter identity")
	}
	if id(base) == id(wider) {
		t.Error("a window edit kept the counter identity")
	}
}
