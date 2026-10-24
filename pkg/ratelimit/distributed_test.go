//go:build integration

package ratelimit

// distributed_test.go — integration tests requiring a real Redis instance.
// Run with: make test-integration.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/metrics"
)

// TestDistributed_Reserve_TwoLimiters: correctness gate.
// 1000 concurrent goroutines split across 2 Limiter instances sharing one Redis.
// Budget = 200 RPM. Asserts admitted ∈ [195,200].
func TestDistributed_Reserve_TwoLimiters(t *testing.T) {
	cfg := kvtest.Config(t)
	s1 := kvtest.Connect(t, cfg)
	s2 := kvtest.Connect(t, cfg)

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
	// Capped below the kv pool size: 1000 calls at once can outwait the pool on a loaded runner and fail as store-unavailable.
	inFlight := make(chan struct{}, 100)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			inFlight <- struct{}{}
			defer func() { <-inFlight }()
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

func redisLimiterFactory(cfg kv.RedisConfig) func(t *testing.T, now *time.Time) *Limiter {
	return func(t *testing.T, now *time.Time) *Limiter {
		clock := func() time.Time { return *now }
		return New(kvtest.Connect(t, cfg), discardLog(), clock)
	}
}

func TestContractLimit_RedisStore(t *testing.T) {
	factory := redisLimiterFactory(kvtest.Config(t))
	runLimiterContractSuite(t, "RedisStore", factory)
}

func TestConcurrencyLease_RedisStore(t *testing.T) {
	runConcurrencyLeaseSuite(t, "RedisStore", redisLimiterFactory(kvtest.Config(t)))
}

func TestCommitSkipRedis(t *testing.T) {
	runCommitSkipSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

func TestCommitGuardRedis(t *testing.T) {
	runCommitGuardSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

func TestReserveOrShedMatchesReserveRedis(t *testing.T) {
	runReserveOrShedMatchesReserve(t, New(kvtest.NewRedis(t), discardLog(), nil))
}

// With the connection held and the one allowed waiter queued, an admission reserve is refused at once and counted as shed.
func TestDistributed_ReserveOrShedRefusedAtWaitBound(t *testing.T) {
	cfg := kvtest.Config(t)
	cfg.PoolTimeout, cfg.MaxPoolWaiters = 500*time.Millisecond, 1
	s := kvtest.ExhaustedPool(t, cfg)
	l := New(s, discardLog(), nil)
	shed := metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVShed)
	before := testutil.ToFloat64(shed)

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_, _ = s.Get(context.Background(), "{t}:queued")
	}()
	deadline := time.Now().Add(time.Second)
	for !s.PoolBusy() {
		if time.Now().After(deadline) {
			t.Fatal("the waiter never queued")
		}
		time.Sleep(2 * time.Millisecond)
	}

	start := time.Now()
	res, err := l.ReserveOrShed(context.Background(), "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)})
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("refusal took %v, want no pool wait", d)
	}
	if res != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ReserveOrShed = %v, %v; want nil, ErrUnavailable", res, err)
	}
	if got := testutil.ToFloat64(shed) - before; got != 1 {
		t.Fatalf("shed counter moved by %v, want 1", got)
	}
	<-waited
}

// A reserve queued on a batch that never got a pooled connection is refused as unavailable and counted, not failed as an undecodable reply.
func TestDistributed_BatchedReservePoolTimeoutRefused(t *testing.T) {
	s := kvtest.ExhaustedPool(t, kvtest.Config(t))
	l := New(s, discardLog(), nil)
	refused := metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVRefused)
	before := testutil.ToFloat64(refused)

	b := kv.NewBatch(s)
	p, err := l.PrepareReserve(b, "policy:p", []Rule{reqRule("r", "r", 10, time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	b.Exec(context.Background())
	if res, err := p.Finish(context.Background()); res != nil || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("batched reserve = %v, %v; want nil, ErrUnavailable", res, err)
	}
	if got := testutil.ToFloat64(refused) - before; got != 1 {
		t.Fatalf("refused counter moved by %v, want 1", got)
	}
}

// TestDistributed_CommitBoth_Parity: against real Redis, committing an inbound
// (policy-scoped) and upstream (hostkey-scoped) reservation via CommitBoth must
// leave the same bucket state as two sequential Commit calls. The two scopes
// carry different hash tags, so this also exercises the pipeline's per-command
// EVALSHA routing on a live server.
func TestDistributed_CommitBoth_Parity(t *testing.T) {
	cfg := kvtest.Config(t)
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

	s1 := kvtest.Connect(t, cfg)
	l1 := New(s1, discardLog(), func() time.Time { return now })
	seq := dump(t, l1, s1, "seq-policy", "hostkey:seq-key", func(l *Limiter, in, up *Reservation) {
		if err := l.Commit(context.Background(), in, obs); err != nil {
			t.Fatalf("commit inbound: %v", err)
		}
		if err := l.Commit(context.Background(), up, obs); err != nil {
			t.Fatalf("commit upstream: %v", err)
		}
	})

	s2 := kvtest.Connect(t, cfg)
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
