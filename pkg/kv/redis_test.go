//go:build integration

package kv_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

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
	ctx := context.Background()
	s, err := kv.NewRedis(ctx, kv.RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---- contract tests — same body runs against MemStore and RedisStore ----

type storeFactory func(t *testing.T) kv.Store

func contractGetSet(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	_, err := s.Get(ctx, "missing")
	if !errors.Is(err, kv.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if err := s.Set(ctx, "k", []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	v, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "hello" {
		t.Fatalf("want hello, got %s", v)
	}
}

func contractIncr(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Incr(ctx, "counter", 1); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	v, err := s.Get(ctx, "counter")
	if err != nil {
		t.Fatal(err)
	}
	if string(v) != "100" {
		t.Fatalf("want 100, got %s", v)
	}
}

func contractTTL(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	if err := s.Set(ctx, "ttl-key", []byte("val"), 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "ttl-key"); err != nil {
		t.Fatalf("expected value before expiry, got %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := s.Get(ctx, "ttl-key"); !errors.Is(err, kv.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after expiry, got %v", err)
	}
}

func contractRange(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	for _, k := range []string{"r:1", "r:2", "other"} {
		if err := s.Set(ctx, k, []byte(k), 0); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Range(ctx, "r:")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
}

func contractExpire(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	if err := s.Set(ctx, "exp-key", []byte("v"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := s.Expire(ctx, "exp-key", 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := s.Get(ctx, "exp-key"); err != nil {
		t.Fatalf("expected key after Expire reset, got %v", err)
	}
	if err := s.Expire(ctx, "no-such", 100*time.Millisecond); !errors.Is(err, kv.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func contractWithLock(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	var (
		mu          sync.Mutex
		counter     int
		interleaved bool
	)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.WithLock(ctx, []string{"x", "y"}, func(ctx context.Context) error {
				mu.Lock()
				counter++
				snap := counter
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
				mu.Lock()
				if counter != snap {
					interleaved = true
				}
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	if interleaved {
		t.Fatal("detected interleaving inside WithLock")
	}
}

// runContractSuite runs every primitive contract against a factory.
func runContractSuite(t *testing.T, name string, factory storeFactory) {
	t.Helper()
	t.Run(name+"/GetSet", func(t *testing.T) { contractGetSet(t, factory) })
	t.Run(name+"/Incr", func(t *testing.T) { contractIncr(t, factory) })
	t.Run(name+"/TTL", func(t *testing.T) { contractTTL(t, factory) })
	t.Run(name+"/Range", func(t *testing.T) { contractRange(t, factory) })
	t.Run(name+"/Expire", func(t *testing.T) { contractExpire(t, factory) })
	t.Run(name+"/WithLock", func(t *testing.T) { contractWithLock(t, factory) })
}

func TestContractMem(t *testing.T) {
	runContractSuite(t, "MemStore", func(t *testing.T) kv.Store {
		s := kv.NewMem()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}

func TestContractRedis(t *testing.T) {
	addr := startRedis(t)
	// Each sub-test needs its own store to avoid key collisions.
	runContractSuite(t, "RedisStore", func(t *testing.T) kv.Store {
		return newRedisStore(t, addr)
	})
}

// ---- RunScript tests ----

func TestRunScriptCacheHit(t *testing.T) {
	addr := startRedis(t)
	s := newRedisStore(t, addr)
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
	addr := startRedis(t)
	s := newRedisStore(t, addr)
	ctx := context.Background()

	const script = `return "hello"`
	// first call loads script
	if _, err := s.RunScript(ctx, "test.noscript", script, nil); err != nil {
		t.Fatal(err)
	}

	// flush all scripts from Redis so EVALSHA will get NOSCRIPT
	rawClient := redis.NewClient(&redis.Options{Addr: addr})
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

// TestWithLockContention spawns goroutines that fight over [A,B] and [B,A].
// Sorted keys prevent deadlock; only one may hold the lock at a time.
func TestWithLockContention(t *testing.T) {
	addr := startRedis(t)
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
			s := newRedisStore(t, addr)
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

// Sentinel: skipped — standalone Redis is the default tested topology.
// Sentinel topology is exercised in HITL (PER-248).
func TestSentinelSkipped(t *testing.T) {
	t.Skip("Sentinel exercised in HITL PER-248; standalone is default integration topology")
}
