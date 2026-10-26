package ratelimit

import (
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

func benchLimiterRules() []Rule {
	return []Rule{
		{Key: "Policy:prod-policy:rl-1:requests", Name: "requests on rl-1", Meter: "requests", Amount: 1 << 40, Window: time.Hour, Strategy: StrategyTokenBucket},
		{Key: "Policy:prod-policy:rl-1:tokens", Name: "tokens on rl-1", Meter: "tokens", Amount: 1 << 40, Window: time.Hour, Strategy: StrategyTokenBucket},
		{Key: "Policy:prod-policy:rl-1:tokens.input", Name: "tokens.input on rl-1", Meter: "tokens.input", Amount: 1 << 40, Window: time.Hour, Strategy: StrategyTokenBucket},
	}
}

func benchLimiter() *Limiter {
	now := time.Date(2026, 10, 26, 10, 30, 15, 0, time.UTC)
	return New(kv.NewMem(), slog.New(slog.DiscardHandler), func() time.Time { return now })
}

// BenchmarkBuildReserveCall covers key building and argument encoding for one reserve script call.
func BenchmarkBuildReserveCall(b *testing.B) {
	l := benchLimiter()
	rules := benchLimiterRules()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.buildReserveCall("policy:prod-policy", rules, "res-1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildCommitCall covers key building and argument encoding for one commit script call.
func BenchmarkBuildCommitCall(b *testing.B) {
	l := benchLimiter()
	rules := benchLimiterRules()
	res := &Reservation{ID: "res-1", scope: "policy:prod-policy", rules: rules, tokRules: rules[1:], tbRules: rules[:1]}
	obs := Observations{Tokens: map[string]int64{"input": 120, "output": 40}}
	now := l.clock()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.buildCommitCall(res, obs, now); err != nil {
			b.Fatal(err)
		}
	}
}
