//go:build integration

package ratelimit

// distributed_test.go — integration tests requiring a real Redis instance.
// Run with: go test -tags integration ./pkg/ratelimit/...

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/wyolet/relay/pkg/kv"
)

func startRedis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	req := tc.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForLog("Ready to accept connections"),
	}
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	return fmt.Sprintf("%s:%s", host, port.Port())
}

func newRedisStore(t *testing.T, addr string) *kv.Redis {
	t.Helper()
	s, err := kv.NewRedis(context.Background(), kv.RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestDistributed_Reserve_TwoLimiters: correctness gate.
// 1000 concurrent goroutines split across 2 Limiter instances sharing one Redis.
// Budget = 200 RPM. Asserts admitted ∈ [195,200].
func TestDistributed_Reserve_TwoLimiters(t *testing.T) {
	addr := startRedis(t)
	s1 := newRedisStore(t, addr)
	s2 := newRedisStore(t, addr)

	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	clock := func() time.Time { return now }
	log := discardLog()

	l1 := New(s1, log, clock)
	l2 := New(s2, log, clock)

	const budget = 200
	const goroutines = 1000
	rule := Rule{
		Key:      "Route:test-route:rl-requests",
		Name:     "requests",
		Meter:    "requests",
		Strategy: StrategySlidingWindow,
		Amount:   budget,
		Window:   time.Minute,
	}
	rules := []Rule{rule}

	var admitted atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			l := l1
			if i%2 == 0 {
				l = l2
			}
			res, err := l.Reserve(context.Background(), "test-policy", rules)
			if err != nil {
				if !errors.Is(err, ErrExceeded) {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			admitted.Add(1)
			_ = l.Commit(context.Background(), res, Observations{})
		}(i)
	}
	wg.Wait()

	n := admitted.Load()
	t.Logf("admitted=%d (budget=%d)", n, budget)
	if n > budget {
		t.Fatalf("OVER BUDGET: admitted=%d > budget=%d", n, budget)
	}
	if n < 195 {
		t.Fatalf("admitted=%d is too low (expected ≥195); possible bug", n)
	}
}

func redisLimiterFactory(addr string) func(t *testing.T, now *time.Time) *Limiter {
	return func(t *testing.T, now *time.Time) *Limiter {
		s, err := kv.NewRedis(context.Background(), kv.RedisConfig{Addr: addr})
		if err != nil {
			t.Fatalf("NewRedis: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		clock := func() time.Time { return *now }
		return New(s, discardLog(), clock)
	}
}

func TestContractLimit_RedisStore(t *testing.T) {
	addr := startRedis(t)
	factory := redisLimiterFactory(addr)
	runLimiterContractSuite(t, "RedisStore", factory)
}

// TestDistributed_CommitBoth_Parity: against real Redis, committing an inbound
// (policy-scoped) and upstream (hostkey-scoped) reservation via CommitBoth must
// leave the same bucket state as two sequential Commit calls. The two scopes
// carry different hash tags, so this also exercises the pipeline's per-command
// EVALSHA routing on a live server.
func TestDistributed_CommitBoth_Parity(t *testing.T) {
	addr := startRedis(t)
	now := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	window := time.Minute
	inbound := []Rule{
		reqRule("inb:req", "inbound requests", 100, window),
		tokRule("inb:tok", "inbound tokens", 1_000_000, window),
		conRule("inb:con", "inbound concurrency", 10, window),
	}
	upstream := []Rule{
		{Key: "ups:req", Name: "upstream requests", Meter: "requests", Strategy: StrategyTokenBucket, Amount: 100, Window: window},
		tokRule("ups:tok", "upstream tokens", 1_000_000, window),
		conRule("ups:con", "upstream concurrency", 10, window),
	}
	obs := Observations{Tokens: map[string]int64{"input": 300, "output": 200}}

	// Distinct scopes per path so the two runs do not collide in one Redis.
	dump := func(t *testing.T, l *Limiter, s *kv.Redis, inScope, upScope string, commit func(l *Limiter, in, up *Reservation)) map[string]string {
		in, err := l.Reserve(context.Background(), inScope, inbound)
		if err != nil {
			t.Fatalf("reserve inbound: %v", err)
		}
		up, err := l.Reserve(context.Background(), upScope, upstream)
		if err != nil {
			t.Fatalf("reserve upstream: %v", err)
		}
		commit(l, in, up)
		entries, err := s.Range(context.Background(), "limit:")
		if err != nil {
			t.Fatalf("range: %v", err)
		}
		out := map[string]string{}
		for _, e := range entries {
			k := e.Key
			// Both runs share one Redis; keep only this run's own scopes.
			if !strings.Contains(k, inScope) && !strings.Contains(k, upScope) {
				continue
			}
			// Normalize the per-run scope prefix and drop random guard keys.
			k = strings.ReplaceAll(k, upScope, "UP")
			k = strings.ReplaceAll(k, inScope, "IN")
			if strings.Contains(k, ":committed:") {
				continue
			}
			out[k] = string(e.Value)
		}
		return out
	}

	s1 := newRedisStore(t, addr)
	l1 := New(s1, discardLog(), func() time.Time { return now })
	seq := dump(t, l1, s1, "seq-policy", "hostkey:seq-key", func(l *Limiter, in, up *Reservation) {
		if err := l.Commit(context.Background(), in, obs); err != nil {
			t.Fatalf("commit inbound: %v", err)
		}
		if err := l.Commit(context.Background(), up, obs); err != nil {
			t.Fatalf("commit upstream: %v", err)
		}
	})

	s2 := newRedisStore(t, addr)
	l2 := New(s2, discardLog(), func() time.Time { return now })
	batch := dump(t, l2, s2, "batch-policy", "hostkey:batch-key", func(l *Limiter, in, up *Reservation) {
		if err := l.CommitBoth(context.Background(), in, up, obs); err != nil {
			t.Fatalf("CommitBoth: %v", err)
		}
	})

	if len(seq) != len(batch) {
		t.Fatalf("key count: sequential=%d batched=%d", len(seq), len(batch))
	}
	for k, v := range seq {
		if batch[k] != v {
			t.Errorf("key %q: sequential=%q batched=%q", k, v, batch[k])
		}
	}
}
