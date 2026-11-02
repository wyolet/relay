package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPeek_Mem(t *testing.T) { runPeekSuite(t, "MemStore", newTestLimiter) }

// Subtests may share one store (the Redis factory leases one keyspace per test), so each counts under a scope of its own: t.Name().
func runPeekSuite(t *testing.T, name string, factory func(t *testing.T, now *time.Time) *Limiter) {
	ctx := context.Background()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	t.Run(name+"/CountsWhatReserveCounted", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		rules := []Rule{
			{Key: "fw", Meter: "requests", Strategy: StrategyFixedWindow, Amount: 10, Window: time.Minute},
			{Key: "sw", Meter: "requests", Strategy: StrategySlidingWindow, Amount: 10, Window: time.Minute},
			{Key: "tb", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 10, Window: time.Minute},
			{Key: "lb", Meter: "requests", Strategy: StrategyLeakyBucket, Amount: 10, Window: time.Minute},
			{Key: "ss", Meter: "requests", Strategy: StrategySessionWindow, Amount: 10, Window: time.Minute},
			{Key: "tok", Meter: "tokens", Strategy: StrategySlidingWindow, Amount: 1000, Window: time.Minute},
			{Key: "con", Meter: "concurrency", Amount: 5, Window: time.Minute},
			spendRule("hard", 1_000_000, false),
			spendRule("alert", 1_000_000, true),
		}
		s := t.Name()
		var held []*Reservation
		for range 3 {
			res, err := l.Reserve(ctx, s, rules)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, res)
		}
		if err := l.Commit(ctx, held[0], Observations{Tokens: map[string]int64{"input": 40, "output": 2}, CostNanos: 700}); err != nil {
			t.Fatal(err)
		}
		got := peekOne(t, l, s, rules)
		want := []int64{3, 3, 3, 3, 3, 42, 2, 700, 700}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: peek = %d, want %d", rules[i].Key, got[i], want[i])
			}
		}
	})

	t.Run(name+"/FollowsTime", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		rules := []Rule{
			{Key: "sw", Meter: "requests", Strategy: StrategySlidingWindow, Amount: 10, Window: time.Minute},
			{Key: "tb", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 10, Window: 10 * time.Second},
			{Key: "lb", Meter: "requests", Strategy: StrategyLeakyBucket, Amount: 10, Window: 10 * time.Second},
			{Key: "ss", Meter: "requests", Strategy: StrategySessionWindow, Amount: 10, Window: 30 * time.Second},
		}
		s := t.Name()
		for range 5 {
			if _, err := l.Reserve(ctx, s, rules); err != nil {
				t.Fatal(err)
			}
		}
		now = start.Add(2 * time.Second)
		// A bucket of 10 per 10 s refills 2 in 2 s; a leaky one drains 2.
		if got := peekOne(t, l, s, rules); got[0] != 5 || got[1] != 3 || got[2] != 3 || got[3] != 5 {
			t.Errorf("after 2s: %v, want [5 3 3 5]", got)
		}
		now = start.Add(90 * time.Second)
		// Half way into the next minute the previous minute's 5 weigh half, rounded up; buckets refill, the session ended.
		if got := peekOne(t, l, s, rules); got[0] != 3 || got[1] != 0 || got[2] != 0 || got[3] != 0 {
			t.Errorf("after 90s: %v, want [3 0 0 0]", got)
		}
	})

	t.Run(name+"/WritesNothing", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		rules := []Rule{
			{Key: "fw", Meter: "requests", Strategy: StrategyFixedWindow, Amount: 2, Window: time.Minute},
			{Key: "tb", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 2, Window: time.Minute},
			{Key: "con", Meter: "concurrency", Amount: 2, Window: time.Minute},
		}
		s := t.Name()
		for range 5 {
			if got := peekOne(t, l, s, rules); got[0] != 0 || got[1] != 0 || got[2] != 0 {
				t.Fatalf("fresh peek = %v", got)
			}
		}
		for i := range 2 {
			if _, err := l.Reserve(ctx, s, rules); err != nil {
				t.Fatalf("reserve %d after peeks: %v", i, err)
			}
		}
		if _, err := l.Reserve(ctx, s, rules); err == nil {
			t.Fatal("third reserve passed: peeks must not count")
		}
	})

	t.Run(name+"/ScopesApart", func(t *testing.T) {
		now := start
		l := factory(t, &now)
		r := Rule{Key: "fw", Meter: "requests", Strategy: StrategyFixedWindow, Amount: 10, Window: time.Minute}
		a, b := t.Name()+"/a", t.Name()+"/b"
		if _, err := l.Reserve(ctx, a, []Rule{r}); err != nil {
			t.Fatal(err)
		}
		got, err := l.Peek(ctx, []PeekScope{{Scope: a, Rules: []Rule{r}}, {Scope: b, Rules: []Rule{r}}, {Scope: t.Name() + "/c"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 || got[0][0] != 1 || got[1][0] != 0 || len(got[2]) != 0 {
			t.Errorf("peek = %v, want [[1] [0] []]", got)
		}
	})
}

func peekOne(t *testing.T, l *Limiter, scope string, rules []Rule) []int64 {
	t.Helper()
	got, err := l.Peek(context.Background(), []PeekScope{{Scope: scope, Rules: rules}})
	if err != nil {
		t.Fatal(err)
	}
	return got[0]
}

// Every scope is read in one batched round trip.
func TestPeek_OneRoundTrip(t *testing.T) {
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	RegisterScripts(mem)
	rec := kvtest.NewRecording(mem)
	l := New(rec, discardLog(), nil)
	r := Rule{Key: "fw", Meter: "requests", Strategy: StrategyFixedWindow, Amount: 10, Window: time.Minute}
	if _, err := l.Peek(context.Background(), []PeekScope{{Scope: "a", Rules: []Rule{r}}, {Scope: "b", Rules: []Rule{r}}}); err != nil {
		t.Fatal(err)
	}
	if n := rec.RoundTrips(); n != 1 {
		t.Errorf("round trips = %d, want 1", n)
	}
	if calls := rec.Named("limit.peek"); len(calls) != 2 {
		t.Errorf("peek scripts = %d, want 2", len(calls))
	}
}
