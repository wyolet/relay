package lua

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPeek_Mem(t *testing.T) { runPeekSuite(t, memStore) }

func runPeekSuite(t *testing.T, store newStore) {
	ctx := context.Background()
	start := time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)

	t.Run("ReadsEveryGroupInOneRoundTrip", func(t *testing.T) {
		now := start
		rec := kvtest.NewRecording(store(t))
		l := newLimiter(rec, &now)
		caller := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 10), "project:x")
		up := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 5), "a")
		spend := budgetCounter("2", budget.PeriodMonth, budget.ModeHard)
		for range 3 {
			admit(t, l, caller, up, spend).Settle(limits.Usage{CostNanos: usd / 4})
		}
		before := rec.RoundTrips()
		got, err := l.Peek(ctx, []limits.Counter{caller, up, spend})
		if err != nil {
			t.Fatal(err)
		}
		if trips := rec.RoundTrips() - before; trips != 1 {
			t.Errorf("peek made %d round trips, want 1", trips)
		}
		want := []limits.Reading{{Used: 3, Amount: 10}, {Used: 3, Amount: 5}, {Used: 3 * usd / 4, Amount: 2 * usd}}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("counter %d: %+v, want %+v", i, got[i], want[i])
			}
		}
		if r := got[2].Remaining(); r != 2*usd-3*usd/4 {
			t.Errorf("budget remaining = %d", r)
		}
	})

	t.Run("EachReadsTheLaneBucket", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		shared := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 10), "team:t1")
		one, two := shared.ForLane("account:l1"), shared.ForLane("account:l2")
		admit(t, l, one)
		admit(t, l, one)
		admit(t, l, two)
		got, err := l.Peek(ctx, []limits.Counter{shared, one, two})
		if err != nil {
			t.Fatal(err)
		}
		if got[0].Used != 0 || got[1].Used != 2 || got[2].Used != 1 {
			t.Errorf("shared, l1, l2 = %d, %d, %d; want 0, 2, 1", got[0].Used, got[1].Used, got[2].Used)
		}
	})

	t.Run("CountsNothing", func(t *testing.T) {
		now := start
		l := newLimiter(store(t), &now)
		c := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, 2), "project:x")
		for range 3 {
			if _, err := l.Peek(ctx, []limits.Counter{c}); err != nil {
				t.Fatal(err)
			}
		}
		admit(t, l, c)
		admit(t, l, c)
		_ = refused(t, l, c)
	})
}
