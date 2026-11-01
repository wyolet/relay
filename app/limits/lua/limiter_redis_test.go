//go:build integration

package lua

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/sdk/usage"
)

func redisStore(t testing.TB) kvtest.ScriptStore { return kvtest.NewRedis(t) }

func TestLimiter_Redis(t *testing.T) { runSuite(t, redisStore) }

func TestLimiter_RedisKeysExpire(t *testing.T) {
	cfg := kvtest.Config(t)
	now := time.Now()
	l := newLimiter(kvtest.Connect(t, cfg), &now)
	cs := []limits.Counter{
		callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyTokenBucket, 10), "project:x"),
		callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyLeakyBucket, 10), "project:x"),
		callerCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySessionWindow, 10), "project:x"),
		callerCounter(rule(ratelimit.MeterTokens, ratelimit.StrategySlidingWindow, 1000), "project:x"),
		callerCounter(rule(ratelimit.MeterConcurrency, ratelimit.StrategySlidingWindow, 10), "project:x"),
		budgetCounter("10", budget.PeriodMonth, budget.ModeHard),
		upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategyFixedWindow, 10), "a"),
		upstreamCounter(rule(ratelimit.MeterRequests, ratelimit.StrategySlidingWindow, 10), "a"),
	}
	admit(t, l, cs...).Settle(limits.Usage{Tokens: usage.Tokens{"input": 5}, CostNanos: 100})
	admit(t, l, cs...) // holds a lease

	raw := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
	defer raw.Close()
	ctx := context.Background()
	keys, err := raw.Keys(ctx, "*").Result()
	if err != nil || len(keys) == 0 {
		t.Fatalf("Keys = %v, %v", keys, err)
	}
	for _, k := range keys {
		if ttl := raw.PTTL(ctx, k).Val(); ttl <= 0 {
			t.Errorf("key %s has no TTL (%v)", k, ttl)
		}
	}
}

func BenchmarkAdmitSettle_Redis(b *testing.B) { benchmarkAdmitSettle(b, redisStore(b)) }
