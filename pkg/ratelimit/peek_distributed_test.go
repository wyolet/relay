//go:build integration

package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPeek_RedisStore(t *testing.T) {
	runPeekSuite(t, "RedisStore", redisLimiterFactory(kvtest.Config(t)))
}

// A peek creates no key and leaves every TTL where admission put it.
func TestPeek_RedisLeavesKeysAndTTLs(t *testing.T) {
	cfg := kvtest.Config(t)
	now := time.Now()
	l := New(kvtest.Connect(t, cfg), discardLog(), func() time.Time { return now })
	ctx := context.Background()
	rules := []Rule{
		{Key: "fw", Meter: "requests", Strategy: StrategyFixedWindow, Amount: 10, Window: time.Minute},
		{Key: "tb", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 10, Window: time.Minute},
		{Key: "con", Meter: "concurrency", Amount: 10, Window: time.Minute},
		spendRule("b", 1_000_000, false),
	}
	raw := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
	defer raw.Close()

	if _, err := l.Peek(ctx, []PeekScope{{Scope: "fresh", Rules: rules}}); err != nil {
		t.Fatal(err)
	}
	if keys := raw.Keys(ctx, "*").Val(); len(keys) != 0 {
		t.Fatalf("peek of untouched counters created %v", keys)
	}

	if _, err := l.Reserve(ctx, "s", rules); err != nil {
		t.Fatal(err)
	}
	keys := raw.Keys(ctx, "*").Val()
	before := map[string]time.Duration{}
	for _, k := range keys {
		before[k] = raw.PTTL(ctx, k).Val()
	}
	time.Sleep(20 * time.Millisecond)
	if _, err := l.Peek(ctx, []PeekScope{{Scope: "s", Rules: rules}}); err != nil {
		t.Fatal(err)
	}
	after := raw.Keys(ctx, "*").Val()
	if len(after) != len(keys) {
		t.Fatalf("keys %v became %v", keys, after)
	}
	for _, k := range keys {
		if ttl := raw.PTTL(ctx, k).Val(); ttl > before[k] {
			t.Errorf("%s: TTL %v grew to %v", k, before[k], ttl)
		}
	}
}
