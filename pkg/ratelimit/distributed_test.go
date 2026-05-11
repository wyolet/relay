//go:build integration

package ratelimit

// distributed_test.go — integration tests requiring a real Redis instance.
// Run with: go test -tags integration ./pkg/ratelimit/...

import (
	"context"
	"errors"
	"fmt"
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
