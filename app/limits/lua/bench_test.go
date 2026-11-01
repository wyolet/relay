package lua

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/sdk/usage"
)

// benchmarkAdmitSettle admits and settles a typical request: three caller counters and a hard budget in the caller group, two counters in the account group. It reports store round trips per request.
func benchmarkAdmitSettle(b *testing.B, s kvtest.ScriptStore) {
	rec := kvtest.NewRecording(s)
	now := time.Now()
	l := newLimiter(rec, &now)
	const many = 1 << 40
	cs := []limits.Counter{
		callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, many), "project:x"),
		callerCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, many), "project:x"),
		callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, many), "project:x"),
		budgetCounter("1000000000", budget.PeriodMonth, budget.ModeHard),
		upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, many), "a"),
		upstreamCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, many), "a"),
	}
	actual := limits.Usage{Tokens: usage.Tokens{"input": 300, "output": 200}, CostNanos: 1500}
	b.ReportAllocs()
	for b.Loop() {
		res, err := l.Admit(b.Context(), cs, limits.Usage{})
		if err != nil {
			b.Fatal(err)
		}
		res.Settle(actual)
	}
	b.ReportMetric(float64(rec.RoundTrips())/float64(b.N), "trips/op")
}

func BenchmarkAdmitSettle_Mem(b *testing.B) { benchmarkAdmitSettle(b, memStore(b)) }
