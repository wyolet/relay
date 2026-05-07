//go:build integration

package ratelimit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
)

// startRedis launches a redis:7-alpine container and returns (addr, cleanup).
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func makeRule(meter catalog.Meter, amount int64, window time.Duration) catalog.ResolvedRule {
	return makeRuleNamed(meter, amount, window, "test-route")
}

func makeRuleNamed(meter catalog.Meter, amount int64, window time.Duration, routeName string) catalog.ResolvedRule {
	return catalog.ResolvedRule{
		ParentKind: catalog.KindRoute,
		ParentName: routeName,
		Meter:      meter,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "rl-" + string(meter)},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   window,
				Amount:   amount,
			},
		},
	}
}

// TestDistributed_Reserve_TwoLimiters: the correctness gate.
// 1000 concurrent goroutines split across 2 Limiter instances sharing one Redis.
// Budget = 200 RPM. Asserts admitted ∈ [195,200].
func TestDistributed_Reserve_TwoLimiters(t *testing.T) {
	addr := startRedis(t)
	s1 := newRedisStore(t, addr)
	s2 := newRedisStore(t, addr)

	now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
	clock := func() time.Time { return now }
	log := discardLogger()

	l1 := ratelimit.New(s1, log, clock)
	l2 := ratelimit.New(s2, log, clock)

	const budget = 200
	const goroutines = 1000
	rule := makeRule(catalog.MeterRequests, budget, time.Minute)
	rules := []catalog.ResolvedRule{rule}

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
			res, err := l.Reserve(context.Background(), "test-pool", rules)
			if err != nil {
				if !errors.Is(err, ratelimit.ErrExceeded) {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			admitted.Add(1)
			_ = l.Commit(context.Background(), res, ratelimit.Observations{})
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

// ---- contract tests: same bodies as MemStore, now running against RedisStore ----

type storeFactoryFn func(t *testing.T) *ratelimit.Limiter

// redisLimiterFactory returns a factory that creates a fresh RedisStore per call.
// Each call creates a new store so sub-tests are isolated.
func redisLimiterFactory(addr string) func(t *testing.T, now *time.Time) *ratelimit.Limiter {
	return func(t *testing.T, now *time.Time) *ratelimit.Limiter {
		s, err := kv.NewRedis(context.Background(), kv.RedisConfig{Addr: addr})
		if err != nil {
			t.Fatalf("NewRedis: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		clock := func() time.Time { return *now }
		return ratelimit.New(s, discardLogger(), clock)
	}
}

func memLimiterFactory(t *testing.T, now *time.Time) *ratelimit.Limiter {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	clock := func() time.Time { return *now }
	return ratelimit.New(s, discardLogger(), clock)
}

func runLimiterContractSuite(t *testing.T, name string, factory func(t *testing.T, now *time.Time) *ratelimit.Limiter) {
	t.Run(name+"/Requests_HappyPath", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := makeRuleNamed(catalog.MeterRequests, 10, time.Minute, "route-req-happy")
		rules := []catalog.ResolvedRule{rule}
		for i := 0; i < 10; i++ {
			res, err := l.Reserve(ctx, "test-pool", rules)
			if err != nil {
				t.Fatalf("reserve %d: %v", i+1, err)
			}
			_ = l.Commit(ctx, res, ratelimit.Observations{})
		}
		_, err := l.Reserve(ctx, "test-pool", rules)
		if !errors.Is(err, ratelimit.ErrExceeded) {
			t.Fatalf("expected ErrExceeded on 11th reserve, got %v", err)
		}
	})

	t.Run(name+"/Concurrency_BudgetCap", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := makeRuleNamed(catalog.MeterConcurrency, 3, time.Minute, "route-con-cap")
		rules := []catalog.ResolvedRule{rule}
		var r [3]*ratelimit.Reservation
		for i := 0; i < 3; i++ {
			res, err := l.Reserve(ctx, "test-pool", rules)
			if err != nil {
				t.Fatalf("reserve %d: %v", i+1, err)
			}
			r[i] = res
		}
		_, err := l.Reserve(ctx, "test-pool", rules)
		if !errors.Is(err, ratelimit.ErrExceeded) {
			t.Fatalf("expected ErrExceeded on 4th, got %v", err)
		}
		if err := l.Commit(ctx, r[0], ratelimit.Observations{}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		res, err := l.Reserve(ctx, "test-pool", rules)
		if err != nil {
			t.Fatalf("reserve after commit: %v", err)
		}
		_ = l.Commit(ctx, res, ratelimit.Observations{})
	})

	t.Run(name+"/Tokens_PostHoc", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule := makeRuleNamed(catalog.MeterTokens, 100, time.Minute, "route-tok-posthoc")
		rules := []catalog.ResolvedRule{rule}
		var reservations [5]*ratelimit.Reservation
		for i := 0; i < 5; i++ {
			res, err := l.Reserve(ctx, "test-pool", rules)
			if err != nil {
				t.Fatalf("reserve %d: %v", i+1, err)
			}
			reservations[i] = res
		}
		for i, res := range reservations {
			if err := l.Commit(ctx, res, ratelimit.Observations{Tokens: 20}); err != nil {
				t.Fatalf("commit %d: %v", i+1, err)
			}
		}
		_, err := l.Reserve(ctx, "test-pool", rules)
		if !errors.Is(err, ratelimit.ErrExceeded) {
			t.Fatalf("expected ErrExceeded after 100 tokens, got %v", err)
		}
	})

	t.Run(name+"/IdempotentCommit", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		// budget=1: Reserve occupies the slot; Commit releases it (concurrency→0).
		// Duplicate Commit must not double-decrement (would go to -1).
		rule := makeRuleNamed(catalog.MeterConcurrency, 1, time.Minute, "route-idem-commit")
		rules := []catalog.ResolvedRule{rule}
		res, err := l.Reserve(ctx, "test-pool", rules)
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		if err := l.Commit(ctx, res, ratelimit.Observations{Tokens: 50}); err != nil {
			t.Fatalf("commit 1: %v", err)
		}
		// Duplicate commit — must be no-op.
		if err := l.Commit(ctx, res, ratelimit.Observations{Tokens: 50}); err != nil {
			t.Fatalf("commit 2: %v", err)
		}
		// Concurrency should be 1 (slot fully released once; counter at 0 → remaining=1).
		rem, err := l.RemainingByMeter(ctx, "test-pool", rules)
		if err != nil {
			t.Fatalf("remaining: %v", err)
		}
		if rem[catalog.MeterConcurrency] != 1 {
			t.Fatalf("expected concurrency remaining=1 after single commit, got %d", rem[catalog.MeterConcurrency])
		}
	})

	t.Run(name+"/Rollback_FirstViolation", func(t *testing.T) {
		now := time.Date(2024, 1, 1, 0, 0, 30, 0, time.UTC)
		l := factory(t, &now)
		ctx := context.Background()
		rule0 := makeRuleNamed(catalog.MeterRequests, 100, time.Minute, "route-rollback")
		rule0.RateLimit.Metadata.Name = "rl-rule0"
		rule1 := makeRuleNamed(catalog.MeterConcurrency, 0, time.Minute, "route-rollback")
		rule1.RateLimit.Metadata.Name = "rl-rule1"
		rules := []catalog.ResolvedRule{rule0, rule1}

		_, err := l.Reserve(ctx, "test-pool", rules)
		if !errors.Is(err, ratelimit.ErrExceeded) {
			t.Fatalf("expected exceeded, got %v", err)
		}
		var ee *ratelimit.ExceededError
		errors.As(err, &ee)
		if ee.Rule.RateLimit.Metadata.Name != "rl-rule1" {
			t.Fatalf("expected rule1 to be violated, got %s", ee.Rule.RateLimit.Metadata.Name)
		}
		rem, err := l.RemainingByMeter(ctx, "test-pool", []catalog.ResolvedRule{rule0})
		if err != nil {
			t.Fatalf("remaining: %v", err)
		}
		if rem[catalog.MeterRequests] != 100 {
			t.Fatalf("expected rule0 remaining=100 after rollback, got %d", rem[catalog.MeterRequests])
		}
	})
}

func TestContractLimit_MemStore(t *testing.T) {
	runLimiterContractSuite(t, "MemStore", memLimiterFactory)
}

func TestContractLimit_RedisStore(t *testing.T) {
	addr := startRedis(t)
	factory := redisLimiterFactory(addr)
	runLimiterContractSuite(t, "RedisStore", factory)
}
