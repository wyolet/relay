package hybrid

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/sdk/usage"
)

const many = 1 << 40

// localSet is a request whose counters all count locally: caller request and token windows and a hard budget, account request and token windows.
func localSet() []limits.Counter {
	return []limits.Counter{
		callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, many, time.Minute)),
		callerCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, many, time.Minute)),
		budgetCounter("1000000000", budget.ModeHard),
		upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, many, time.Minute)),
		upstreamCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, many, time.Minute)),
	}
}

// mixedSet adds a token bucket and a concurrency lease to the caller group, which are always exact.
func mixedSet() []limits.Counter {
	return append(localSet(),
		callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, many, time.Minute)),
		callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, many, time.Minute)),
	)
}

var benchUsage = limits.Usage{Tokens: usage.Tokens{"input": 300, "output": 200}, CostNanos: 1500}

// benchmarkAdmitSettle admits and settles one request per iteration, synced once before the loop, and reports store round trips per request; syncs during the loop are not counted.
func benchmarkAdmitSettle(b *testing.B, s kvtest.ScriptStore, cs []limits.Counter, useLua bool) {
	now := time.Date(2026, 11, 4, 10, 0, 10, 0, time.UTC)
	rec := kvtest.NewRecording(s)
	clock := func() time.Time { return now }
	exact := lua.New(pkgratelimit.New(rec, discardLog(), clock))
	var lim limits.Limiter = exact
	if !useLua {
		h := New(exact, rec, s, time.Second).WithClock(clock)
		res, err := h.Admit(b.Context(), cs, limits.Usage{})
		if err != nil {
			b.Fatal(err)
		}
		res.Settle(benchUsage)
		h.sync()
		lim = h
	}
	trips := rec.RoundTrips()
	b.ReportAllocs()
	for b.Loop() {
		res, err := lim.Admit(b.Context(), cs, limits.Usage{})
		if err != nil {
			b.Fatal(err)
		}
		res.Settle(benchUsage)
	}
	b.ReportMetric(float64(rec.RoundTrips()-trips)/float64(b.N), "trips/op")
}

// subjectSets is n requests like localSet, each counted for an account of its own.
func subjectSets(n int) [][]limits.Counter {
	sets := make([][]limits.Counter, n)
	for i := range sets {
		subject := "account:a" + strconv.Itoa(i)
		sets[i] = []limits.Counter{
			limits.CallerRule(limits.RungProject, testRateLimit, rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, many, time.Minute), "p1", subject, subject),
			limits.CallerRule(limits.RungProject, testRateLimit, rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, many, time.Minute), "p1", subject, subject),
			upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, many, time.Minute)),
		}
	}
	return sets
}

// benchmarkParallel admits and settles from every goroutine at once, cycling through sets, all counted locally after one sync.
func benchmarkParallel(b *testing.B, sets [][]limits.Counter) {
	now := time.Date(2026, 11, 4, 10, 0, 10, 0, time.UTC)
	s := memStore(b)
	clock := func() time.Time { return now }
	h := New(lua.New(pkgratelimit.New(s, discardLog(), clock)), s, s, time.Second).WithClock(clock)
	for _, cs := range sets {
		res, err := h.Admit(b.Context(), cs, limits.Usage{})
		if err != nil {
			b.Fatal(err)
		}
		res.Settle(benchUsage)
	}
	h.sync()
	var next atomic.Uint64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := next.Add(1) * 7919
		for pb.Next() {
			i++
			res, err := h.Admit(context.Background(), sets[i%uint64(len(sets))], limits.Usage{})
			if err != nil {
				b.Error(err)
				return
			}
			res.Settle(benchUsage)
		}
	})
}

// BenchmarkAdmitSettleParallel_Mem runs every goroutine on the same counters (one hot set) and on 1024 sets that share only an upstream counter.
func BenchmarkAdmitSettleParallel_Mem(b *testing.B) {
	b.Run("same", func(b *testing.B) { benchmarkParallel(b, [][]limits.Counter{localSet()}) })
	b.Run("many", func(b *testing.B) { benchmarkParallel(b, subjectSets(1024)) })
}

func BenchmarkAdmitSettle_Mem(b *testing.B) {
	for _, tc := range []struct {
		name string
		cs   []limits.Counter
	}{{"local", localSet()}, {"mixed", mixedSet()}} {
		b.Run(tc.name+"/lua", func(b *testing.B) { benchmarkAdmitSettle(b, memStore(b), tc.cs, true) })
		b.Run(tc.name+"/hybrid", func(b *testing.B) { benchmarkAdmitSettle(b, memStore(b), tc.cs, false) })
	}
}
