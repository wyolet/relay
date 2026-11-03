package hybrid

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/sdk/usage"
)

var testRateLimit = &ratelimit.RateLimit{Meta: meta.Metadata{ID: "rl1"}}

func rule(meter ratelimit.Meter, strategy ratelimit.Strategy, amount int64, window time.Duration) ratelimit.Rule {
	return ratelimit.Rule{Meter: meter, Strategy: strategy, Amount: amount, Window: ratelimit.Window(window)}
}

func callerCounter(r ratelimit.Rule) limits.Counter {
	return limits.CallerRule(limits.RungProject, testRateLimit, r, "p1", "project:x", "team:t1")
}

func upstreamCounter(r ratelimit.Rule) limits.Counter {
	return limits.UpstreamRule(limits.RungAccount, testRateLimit, r, "p2", "account:a", "", "account:a")
}

func budgetCounter(amountUSD string, mode budget.Mode) limits.Counter {
	b := &budget.Budget{Meta: meta.Metadata{ID: "b1"}, Spec: budget.Spec{AmountUSD: amountUSD, Period: budget.PeriodMonth, Mode: mode}}
	return limits.BudgetCounter(limits.RungTeam, b, "p1", "team:t1", "team:t1")
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type newStore func(t testing.TB) kvtest.ScriptStore

func memStore(t testing.TB) kvtest.ScriptStore {
	m := kv.NewMem()
	t.Cleanup(func() { _ = m.Close() })
	pkgratelimit.RegisterScripts(m)
	return m
}

// pod is one relay process: a hybrid limiter with its own table on a store the pods share, admitting and syncing on rec so a test can count round trips.
type pod struct {
	*Limiter
	rec *kvtest.Recording
}

func newPod(s kvtest.ScriptStore, now *time.Time) pod {
	return newPodOn(s, s, now)
}

func newPodOn(admit, syncOn kvtest.ScriptStore, now *time.Time) pod {
	clock := func() time.Time { return *now }
	rec := kvtest.NewRecording(admit)
	exact := lua.New(pkgratelimit.New(rec, discardLog(), clock))
	return pod{Limiter: New(exact, rec, syncOn, time.Second).WithClock(clock), rec: rec}
}

func (p pod) admit(t *testing.T, cs ...limits.Counter) limits.Reservation {
	t.Helper()
	res, err := p.Admit(context.Background(), cs, limits.Usage{})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return res
}

func (p pod) refused(t *testing.T, cs ...limits.Counter) *limits.Refused {
	t.Helper()
	res, err := p.Admit(context.Background(), cs, limits.Usage{})
	var r *limits.Refused
	if !errors.As(err, &r) || res != nil {
		t.Fatalf("Admit = %v, %v; want a refusal", res, err)
	}
	return r
}

// admitN admits n requests and reports the store round trips they made.
func (p pod) admitN(t *testing.T, n int, cs ...limits.Counter) int {
	t.Helper()
	before := p.rec.RoundTrips()
	for range n {
		p.admit(t, cs...)
	}
	return p.rec.RoundTrips() - before
}

// spread admits n requests at most per a sync interval, syncing and moving the clock one interval on after each batch, so the fleet's spend per interval stays at per.
func (p pod) spread(t *testing.T, now *time.Time, n, per int, cs ...limits.Counter) {
	t.Helper()
	for n > 0 {
		k := min(n, per)
		p.admitN(t, k, cs...)
		n -= k
		*now = now.Add(time.Second)
		p.sync()
	}
}

// stored reads c's counter for the bucket containing at from the store.
func stored(t *testing.T, s kv.Store, c limits.Counter, at time.Time) int64 {
	t.Helper()
	r, err := lua.Rule(&c)
	if err != nil {
		t.Fatal(err)
	}
	start, err := pkgratelimit.BucketStart(r, at)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := pkgratelimit.CounterKey(c.Group, r, start, at)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Get(context.Background(), key)
	if errors.Is(err, kv.ErrNotFound) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (p pod) mode(c limits.Counter) string {
	r, _ := lua.Rule(&c)
	p.mu.Lock()
	defer p.mu.Unlock()
	idx, found, _ := p.tab.find(p.tab.hash(c.Group, c.ID, r.Meter), c.Group, c.ID, r.Meter)
	switch {
	case !found:
		return "untracked"
	case p.usable(&p.tab.slots[idx], p.clock()):
		return "local"
	}
	return "exact"
}

func runSuite(t *testing.T, store newStore) {
	// Ten seconds into a minute, so a test has room before the window ends.
	start := time.Date(2026, 11, 4, 10, 0, 10, 0, time.UTC)
	fixed := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, time.Minute))

	t.Run("LocalAdmitMakesNoStoreCall", func(t *testing.T) {
		now := start
		s := store(t)
		p := newPod(s, &now)
		if trips := p.admitN(t, 1, fixed); trips != 1 {
			t.Fatalf("first admit made %d round trips, want 1 (exact until a sync reads the total)", trips)
		}
		p.sync()
		if trips := p.admitN(t, 100, fixed); trips != 0 {
			t.Fatalf("local admits made %d round trips, want 0", trips)
		}
		if got := stored(t, s, fixed, now); got != 1 {
			t.Fatalf("store holds %d before the sync, want 1", got)
		}
		p.sync()
		if got := stored(t, s, fixed, now); got != 101 {
			t.Fatalf("store holds %d after the sync, want 101", got)
		}
	})

	t.Run("ExactAtEightyPercent", func(t *testing.T) {
		now := start
		p := newPod(store(t), &now)
		p.spread(t, &now, 799, 100, fixed)
		if m := p.mode(fixed); m != "local" {
			t.Fatalf("at 799 of 1000 the counter is %s, want local", m)
		}
		p.spread(t, &now, 1, 100, fixed)
		if m := p.mode(fixed); m != "exact" {
			t.Fatalf("at 800 of 1000 the counter is %s, want exact", m)
		}
		if trips := p.admitN(t, 3, fixed); trips != 3 {
			t.Fatalf("3 exact admits made %d round trips", trips)
		}
	})

	t.Run("ExactWhenOneIntervalCouldFillIt", func(t *testing.T) {
		now := start
		long := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, 10*time.Minute))
		p := newPod(store(t), &now)
		p.admitN(t, 1, long)
		p.sync()
		now = now.Add(time.Second)
		p.admitN(t, 399, long)
		p.sync()
		now = now.Add(time.Second)
		p.admitN(t, 350, long)
		p.sync()
		// 750 counted, 350 in the last interval: 250 left is less than one interval spends, though under 80%.
		if m := p.mode(long); m != "exact" {
			t.Fatalf("with 250 left at 350 per interval the counter is %s, want exact", m)
		}
	})

	t.Run("HandoffCountsOnce", func(t *testing.T) {
		now := start
		s := store(t)
		p := newPod(s, &now)
		p.spread(t, &now, 799, 100, fixed)
		last := p.admit(t, fixed)
		now = now.Add(time.Second)
		p.sync() // 800: exact
		last.Release()
		// The release left -1 unsynced; the exact admit hands it over ahead of its script.
		if trips := p.admitN(t, 1, fixed); trips != 1 {
			t.Fatalf("exact admit with a handoff made %d round trips, want 1", trips)
		}
		if got := stored(t, s, fixed, now); got != 800 {
			t.Fatalf("store holds %d after the handoff, want 800", got)
		}
		p.sync()
		if got := stored(t, s, fixed, now); got != 800 {
			t.Fatalf("store holds %d after the next sync, want 800 (no second push)", got)
		}

		tokens := callerCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 10000, time.Minute))
		p.admit(t, tokens)
		p.sync()
		held := p.admit(t, tokens) // local
		p.admit(t, tokens).Settle(limits.Usage{Tokens: usage.Tokens{"input": 8000}})
		now = now.Add(time.Second)
		p.sync() // 8000 of 10000: exact
		held.Settle(limits.Usage{Tokens: usage.Tokens{"input": 2000, "output": 500}})
		// The settle after the switch is unsynced; the script must see it and refuse at 10,500.
		if r := p.refused(t, tokens); r.Counter.ID != tokens.ID {
			t.Fatalf("refused by %s, want the token counter", r.Counter.ID)
		}
		if got := stored(t, s, tokens, now); got != 10500 {
			t.Fatalf("token store holds %d, want 10500", got)
		}
		p.sync()
		if got := stored(t, s, tokens, now); got != 10500 {
			t.Fatalf("token store holds %d after the next sync, want 10500", got)
		}
	})

	t.Run("PodsOvershootWithinBound", func(t *testing.T) {
		now := start
		s := store(t)
		long := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, 10*time.Minute))
		const pods, perPod = 3, 60
		ps := make([]pod, pods)
		for i := range ps {
			ps[i] = newPod(s, &now)
		}
		admitted := 0
		for range 30 {
			for _, p := range ps {
				for range perPod {
					res, err := p.Admit(context.Background(), []limits.Counter{long}, limits.Usage{})
					var r *limits.Refused
					switch {
					case err == nil:
						admitted++
						res.Settle(limits.Usage{})
					case errors.As(err, &r):
					default:
						t.Fatal(err)
					}
				}
			}
			for _, p := range ps {
				p.sync()
			}
			now = now.Add(time.Second)
		}
		t.Logf("%d pods admitted %d against a cap of 1000", pods, admitted)
		bound := pods * perPod // pods × rate × interval
		if admitted < 1000 || admitted > 1000+bound {
			t.Fatalf("admitted %d against a cap of 1000, want within [1000, %d]", admitted, 1000+bound)
		}
		if got := stored(t, s, long, now); got != int64(admitted) {
			t.Fatalf("store holds %d, admitted %d: counts lost or doubled", got, admitted)
		}
	})

	t.Run("Refunds", func(t *testing.T) {
		now := start
		s := store(t)
		p := newPod(s, &now)
		p.admitN(t, 1, fixed)
		p.sync()
		p.admit(t, fixed).Release()
		p.sync()
		if got := stored(t, s, fixed, now); got != 1 {
			t.Fatalf("store holds %d after a released local admit, want 1", got)
		}

		// A local count is undone when an exact counter of the same request refuses.
		bucket := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 1, time.Minute))
		p.admit(t, fixed, bucket)
		if r := p.refused(t, fixed, bucket); r.Counter.ID != bucket.ID {
			t.Fatalf("refused by %s, want the token bucket", r.Counter.ID)
		}
		p.sync()
		if got := stored(t, s, fixed, now); got != 2 {
			t.Fatalf("store holds %d, want 2: the refused request must not count", got)
		}

		// A local refusal counts nothing and makes no call.
		small := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 100, 10*time.Minute))
		p.admit(t, small)
		p.sync()
		p.admitN(t, 99, small)
		before := p.rec.RoundTrips()
		if r := p.refused(t, small); r.RetryAfter <= 0 {
			t.Fatalf("local refusal has retry-after %v", r.RetryAfter)
		}
		if trips := p.rec.RoundTrips() - before; trips != 0 {
			t.Fatalf("local refusal made %d round trips", trips)
		}
		p.sync()
		if got := stored(t, s, small, now); got != 100 {
			t.Fatalf("store holds %d, want 100", got)
		}
	})

	t.Run("NewWindowReturnsToLocal", func(t *testing.T) {
		now := start
		p := newPod(store(t), &now)
		p.admitN(t, 1, fixed)
		p.sync()
		p.admitN(t, 899, fixed)
		now = start.Add(49*time.Second + 500*time.Millisecond)
		p.sync()
		if m := p.mode(fixed); m != "exact" {
			t.Fatalf("at 900 of 1000 the counter is %s, want exact", m)
		}
		now = start.Add(50*time.Second + 100*time.Millisecond) // next minute
		if trips := p.admitN(t, 10, fixed); trips != 0 {
			t.Fatalf("admits in a new window made %d round trips, want 0", trips)
		}

		// A sliding window still weighs the full previous bucket at its start.
		now = start
		sliding := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, 1000, time.Minute))
		p.admitN(t, 1, sliding)
		p.sync()
		p.admitN(t, 899, sliding)
		now = start.Add(49*time.Second + 500*time.Millisecond)
		p.sync()
		now = start.Add(50*time.Second + 100*time.Millisecond)
		if trips := p.admitN(t, 1, sliding); trips != 1 {
			t.Fatalf("sliding admit at the new bucket made %d round trips, want 1 (900 still weighed)", trips)
		}
		now = now.Add(30 * time.Second)
		p.admitN(t, 1, sliding) // exact: its totals are 30 s old
		now = now.Add(time.Second)
		p.sync()
		if m := p.mode(sliding); m != "local" {
			t.Fatalf("half a window on, weighing about 450, the counter is %s, want local", m)
		}
	})

	t.Run("AlwaysExact", func(t *testing.T) {
		now := start
		p := newPod(store(t), &now)
		for _, c := range []limits.Counter{
			callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 99, time.Minute)),
			callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, time.Second)),
			callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 1000, time.Minute)),
			callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyLeakyBucket, 1000, time.Minute)),
			callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySessionWindow, 1000, time.Minute)),
			callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 1000, time.Minute)),
		} {
			p.admit(t, c).Settle(limits.Usage{})
			p.sync()
			if trips := p.admitN(t, 3, c); trips != 3 {
				t.Errorf("%s: 3 admits made %d round trips, want 3", c.ID, trips)
			}
		}
	})

	t.Run("BudgetsCountLocally", func(t *testing.T) {
		now := start
		s := store(t)
		hard := budgetCounter("0.00001", budget.ModeHard) // 10,000 nanos
		p := newPod(s, &now)
		p.admit(t, hard).Settle(limits.Usage{CostNanos: 100})
		p.sync()
		for range 10 {
			if trips := p.admitN(t, 1, hard); trips != 0 {
				t.Fatalf("local budget admit made %d round trips", trips)
			}
		}
		res := p.admit(t, hard)
		res.Settle(limits.Usage{CostNanos: 7900})
		p.sync()
		if got := stored(t, s, hard, now); got != 8000 {
			t.Fatalf("spend holds %d, want 8000", got)
		}
		if m := p.mode(hard); m != "exact" {
			t.Fatalf("at 80%% spent the budget is %s, want exact", m)
		}
		p.admit(t, hard).Settle(limits.Usage{CostNanos: 2000})
		if r := p.refused(t, hard); r.RetryAfter <= 0 {
			t.Fatalf("spent budget refusal has retry-after %v", r.RetryAfter)
		}
	})

	t.Run("CountsCarryToTheLuaLimiter", func(t *testing.T) {
		now := start
		s := store(t)
		long := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, 200, 10*time.Minute))
		p := newPod(s, &now)
		p.admitN(t, 1, long)
		p.sync()
		p.admitN(t, 149, long)
		p.sync()
		l := lua.New(pkgratelimit.New(s, discardLog(), func() time.Time { return now }))
		for i := range 50 {
			if _, err := l.Admit(context.Background(), []limits.Counter{long}, limits.Usage{}); err != nil {
				t.Fatalf("lua admit %d after 150 hybrid admits: %v", i+1, err)
			}
		}
		if _, err := l.Admit(context.Background(), []limits.Counter{long}, limits.Usage{}); err == nil {
			t.Fatal("lua admitted past the cap the hybrid counted towards")
		}
	})

	t.Run("SyncFailureFailsSafe", func(t *testing.T) {
		now := start
		s := store(t)
		syncOn := &failing{ScriptStore: s}
		p := newPodOn(s, syncOn, &now)
		p.admitN(t, 1, fixed)
		p.sync()
		syncOn.fail.Store(true)
		now = now.Add(time.Second)
		p.admitN(t, 10, fixed)
		p.sync()
		if trips := p.admitN(t, 10, fixed); trips != 0 {
			t.Fatalf("one interval after the last good sync admits made %d round trips, want 0", trips)
		}
		now = now.Add(time.Second + 100*time.Millisecond)
		if trips := p.admitN(t, 1, fixed); trips != 1 {
			t.Fatalf("two intervals after the last good sync an admit made %d round trips, want 1 (exact)", trips)
		}
		// Exact fails closed.
		down := newPodOn(&failing{ScriptStore: s, all: true}, s, &now)
		if _, err := down.Admit(context.Background(), []limits.Counter{fixed}, limits.Usage{}); err == nil {
			t.Fatal("exact admit on a failing store was admitted")
		}
		syncOn.fail.Store(false)
		p.sync()
		// 1 + 20 local, then 1 exact; the exact one's handoff carried the 20.
		if got := stored(t, s, fixed, now); got != 22 {
			t.Fatalf("store holds %d after recovery, want 22", got)
		}
	})

	t.Run("IdleCountersFreed", func(t *testing.T) {
		now := start
		p := newPod(store(t), &now)
		p.admitN(t, 1, fixed)
		p.sync()
		now = now.Add(idleAfter + 2*time.Second)
		p.sync()
		if m := p.mode(fixed); m != "untracked" {
			t.Fatalf("idle counter is %s, want freed", m)
		}
		other := upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, time.Minute))
		p.admitN(t, 1, other)
		if n := len(p.tab.slots); n != 1 {
			t.Fatalf("table holds %d slots, want the freed one reused", n)
		}
	})
}

// failing fails every batched command while fail is set, and every script too when all is set, as a store that did not answer.
type failing struct {
	kvtest.ScriptStore
	fail atomic.Bool
	all  bool
}

func (f *failing) down() bool { return f.all || f.fail.Load() }

func (f *failing) ExecBatch(ctx context.Context, ops []kv.BatchOp) {
	if f.down() {
		for _, op := range ops {
			op.Result.Err = kv.ErrUnavailable
		}
		return
	}
	kv.ExecInOrder(ctx, f.ScriptStore, ops)
}

func (f *failing) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	if f.down() {
		return nil, kv.ErrUnavailable
	}
	return f.ScriptStore.RunScript(ctx, name, script, keys, args...)
}

func TestHybrid_Mem(t *testing.T) { runSuite(t, memStore) }

// The table must stay out of the garbage collector's scan.
func TestSlotHoldsNoPointers(t *testing.T) {
	var walk func(reflect.Type) bool
	walk = func(ty reflect.Type) bool {
		switch ty.Kind() {
		case reflect.Pointer, reflect.String, reflect.Slice, reflect.Map, reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer:
			return true
		case reflect.Struct:
			for i := range ty.NumField() {
				if walk(ty.Field(i).Type) {
					return true
				}
			}
		case reflect.Array:
			return walk(ty.Elem())
		}
		return false
	}
	if walk(reflect.TypeFor[slot]()) {
		t.Fatal("slot holds a pointer")
	}
}

func TestCloseSyncsUnsyncedCounts(t *testing.T) {
	now := time.Date(2026, 11, 4, 10, 0, 10, 0, time.UTC)
	s := memStore(t)
	c := callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 1000, time.Minute))
	p := newPod(s, &now)
	p.admitN(t, 1, c)
	p.sync()
	p.admitN(t, 5, c)
	p.Close()
	if got := stored(t, s, c, now); got != 6 {
		t.Fatalf("store holds %d after Close, want 6", got)
	}
}
