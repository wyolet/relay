package hybrid

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/sdk/usage"
)

func TestPeek_Mem(t *testing.T) { runPeekSuite(t, memStore) }

func runPeekSuite(t *testing.T, store newStore) {
	ctx := context.Background()
	start := time.Date(2026, 11, 4, 10, 0, 10, 0, time.UTC)
	fixed := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, time.Minute))

	peek := func(t *testing.T, p pod, cs ...limits.Counter) []limits.Reading {
		t.Helper()
		got, err := p.Peek(ctx, cs)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	t.Run("LocalViewWithoutStoreCall", func(t *testing.T) {
		now := start
		s := store(t)
		a, b := newPod(s, &now), newPod(s, &now)
		a.admitN(t, 1, fixed)
		a.sync()
		a.admitN(t, 100, fixed)
		trips := a.rec.RoundTrips()
		if got := peek(t, a, fixed)[0]; got.Used != 101 || got.Amount != 1000 {
			t.Fatalf("local peek = %+v, want 101 of 1000", got)
		}
		if n := a.rec.RoundTrips() - trips; n != 0 {
			t.Fatalf("local peek made %d round trips", n)
		}
		// Another pod's counts show once both have synced: the local view lags by an interval.
		b.admitN(t, 1, fixed)
		b.sync()
		b.admitN(t, 49, fixed)
		if got := peek(t, a, fixed)[0].Used; got != 101 {
			t.Fatalf("before the syncs pod a reads %d, want its own 101", got)
		}
		b.sync()
		a.sync()
		if got := peek(t, a, fixed)[0].Used; got != 151 {
			t.Fatalf("after the syncs pod a reads %d, want 151", got)
		}
	})

	t.Run("StoreForWhatIsNotLocal", func(t *testing.T) {
		now := start
		s := store(t)
		a, b := newPod(s, &now), newPod(s, &now)
		bucket := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 1000, time.Minute))
		spend := budgetCounter("10", budget.ModeHard)
		b.admit(t, fixed, bucket, spend).Settle(limits.Usage{Tokens: usage.Tokens{}, CostNanos: 250})
		b.sync()
		trips := a.rec.RoundTrips()
		got := peek(t, a, fixed, bucket, spend)
		if got[0].Used != 1 || got[1].Used != 1 || got[2].Used != 250 || got[2].Amount != 10_000_000_000 {
			t.Fatalf("peek = %+v, want 1, 1 and 250 of 10 USD", got)
		}
		if n := a.rec.RoundTrips() - trips; n != 1 {
			t.Fatalf("store peek made %d round trips, want 1", n)
		}
		if m := a.mode(fixed); m != "untracked" {
			t.Fatalf("a peek left the counter %s, want untracked", m)
		}
	})

	t.Run("NextBucketFromLocalView", func(t *testing.T) {
		now := start
		sliding := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, 1000, time.Minute))
		p := newPod(store(t), &now)
		p.admitN(t, 1, sliding)
		p.sync()
		p.admitN(t, 99, sliding)
		bucket := func() int64 {
			r, _ := lua.Rule(&sliding)
			p.mu.Lock()
			defer p.mu.Unlock()
			idx, _, _ := p.tab.find(p.tab.hash(sliding.Group, sliding.ID, r.Meter), sliding.Group, sliding.ID, r.Meter)
			return p.tab.slots[idx].cur.start
		}
		was := bucket()
		now = start.Add(80 * time.Second) // half of the next minute
		if got := peek(t, p, sliding)[0].Used; got != 50 {
			t.Fatalf("half into the next window = %d, want 50", got)
		}
		if bucket() != was {
			t.Fatal("the peek moved the slot to the next bucket")
		}
	})
}
