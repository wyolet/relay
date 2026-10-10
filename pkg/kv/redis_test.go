//go:build integration

package kv_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestContractRedis(t *testing.T) {
	cfg := kvtest.Config(t)
	// Each sub-test needs its own store to avoid key collisions.
	runContractSuite(t, "RedisStore", func(t *testing.T) kv.Store {
		return kvtest.Connect(t, cfg)
	})
}

// ---- RunScript tests ----

func TestRunScriptCacheHit(t *testing.T) {
	s := kvtest.NewRedis(t)
	ctx := context.Background()

	// simple script: returns "ok"
	const script = `return "ok"`
	b1, err := s.RunScript(ctx, "test.script", script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != "ok" {
		t.Fatalf("want ok, got %s", b1)
	}
	// second call — SHA cached, no SCRIPT LOAD needed
	b2, err := s.RunScript(ctx, "test.script", script, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(b2) != "ok" {
		t.Fatalf("want ok on second call, got %s", b2)
	}
}

func TestRunScriptNOSCRIPTFallback(t *testing.T) {
	cfg := kvtest.Config(t)
	s := kvtest.Connect(t, cfg)
	ctx := context.Background()

	const script = `return "hello"`
	// first call loads script
	if _, err := s.RunScript(ctx, "test.noscript", script, nil); err != nil {
		t.Fatal(err)
	}

	// flush all scripts from Redis so EVALSHA will get NOSCRIPT
	rawClient := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
	t.Cleanup(func() { _ = rawClient.Close() })
	if err := rawClient.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("SCRIPT FLUSH: %v", err)
	}

	// second call should detect NOSCRIPT, reload, succeed
	b, err := s.RunScript(ctx, "test.noscript", script, nil)
	if err != nil {
		t.Fatalf("after SCRIPT FLUSH: %v", err)
	}
	if string(b) != "hello" {
		t.Fatalf("want hello, got %s", b)
	}
}

// TestWithLockContention holds the lock contract across separate clients,
// as two pods would share it: goroutines with a client each fight over
// [A,B] and [B,A], and only one may hold the lock at a time.
func TestWithLockContention(t *testing.T) {
	cfg := kvtest.Config(t)
	ctx := context.Background()

	var (
		mu          sync.Mutex
		held        bool
		doubleEntry bool
		wg          sync.WaitGroup
	)
	const goroutines = 50
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			keys := []string{"lockA", "lockB"}
			if i%2 == 0 {
				keys = []string{"lockB", "lockA"}
			}
			s := kvtest.Connect(t, cfg)
			_ = s.WithLock(ctx, keys, func(ctx context.Context) error {
				mu.Lock()
				if held {
					doubleEntry = true
				}
				held = true
				mu.Unlock()

				time.Sleep(2 * time.Millisecond)

				mu.Lock()
				held = false
				mu.Unlock()
				return nil
			})
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: WithLock goroutines did not finish")
	}
	if doubleEntry {
		t.Fatal("lock exclusion violated: two goroutines held lock simultaneously")
	}
}
