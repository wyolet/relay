package kv_test

// contract_test.go — the behaviour every kv.Store backend must share. The
// Mem leg runs here; the Redis leg (TestContractRedis) is integration-tagged.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/kv"
)

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
	if v, err := s.Get(ctx, "k"); err != nil || string(v) != "hello" {
		t.Fatalf("Get after Set = %q, %v; want hello", v, err)
	}
	if err := s.Set(ctx, "k", []byte("again"), 0); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, "k"); err != nil || string(v) != "again" {
		t.Fatalf("Get after a second Set = %q, %v; want again", v, err)
	}
	if err := s.Del(ctx, "k"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	if _, err := s.Get(ctx, "k"); !errors.Is(err, kv.ErrNotFound) {
		t.Fatalf("Get after Del: err = %v, want ErrNotFound", err)
	}
	if err := s.Del(ctx, "k"); err != nil {
		t.Fatalf("Del of a missing key: %v", err)
	}
}

func contractIncr(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	// Each of 100 racing increments returns a count of its own: none is lost, none is seen twice.
	var wg sync.WaitGroup
	returned := make([]int64, 100)
	for i := range returned {
		wg.Go(func() {
			n, err := s.Incr(ctx, "counter", 1)
			if err != nil {
				t.Error(err)
			}
			returned[i] = n
		})
	}
	wg.Wait()
	slices.Sort(returned)
	for i, n := range returned {
		if n != int64(i+1) {
			t.Fatalf("sorted Incr results = %v, want 1..100", returned)
		}
	}

	if n, err := s.Incr(ctx, "counter", -30); err != nil || n != 70 {
		t.Fatalf("Incr by -30 = %d, %v; want 70", n, err)
	}
	if v, err := s.Get(ctx, "counter"); err != nil || string(v) != "70" {
		t.Fatalf("Get = %q, %v; want 70", v, err)
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
	// An expired key is gone from prefix scans too.
	entries, err := s.Range(ctx, "ttl-")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no entries after expiry, got %d", len(entries))
	}
}

func contractRange(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	for _, k := range []string{"r:2", "r:1", "other", "r"} {
		if err := s.Set(ctx, k, []byte("value of "+k), 0); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.Range(ctx, "r:")
	if err != nil {
		t.Fatal(err)
	}
	want := []kv.Entry{{Key: "r:1", Value: []byte("value of r:1")}, {Key: "r:2", Value: []byte("value of r:2")}}
	if diff := cmp.Diff(want, entries); diff != "" {
		t.Fatalf("Range(r:), in key order (-want +got):\n%s", diff)
	}
	if entries, err := s.Range(ctx, "r:1:"); err != nil || len(entries) != 0 {
		t.Fatalf("Range of a prefix no key has = %v, %v", entries, err)
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

// contractWithLock pins the blocking contract the kv.Store interface
// documents: under contention a WithLock call waits for the lock and then
// runs fn — it never skips fn or surfaces a busy error — and two callers
// naming the same keys in opposite orders neither overlap nor deadlock.
func contractWithLock(t *testing.T, factory storeFactory) {
	t.Helper()
	ctx := context.Background()
	s := factory(t)

	const goroutines = 8
	var (
		ran      atomic.Int64
		inside   atomic.Int64
		overlaps atomic.Int64
		errs     = make([]error, goroutines)
		start    = make(chan struct{})
		wg       sync.WaitGroup
	)
	for i := 0; i < goroutines; i++ {
		keys := []string{"{wlc}:x", "{wlc}:y"}
		if i%2 == 1 {
			keys = []string{"{wlc}:y", "{wlc}:x"}
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.WithLock(ctx, keys, func(context.Context) error {
				if inside.Add(1) > 1 {
					overlaps.Add(1)
				}
				time.Sleep(15 * time.Millisecond) // force a contention window
				inside.Add(-1)
				ran.Add(1)
				return nil
			})
		}(i)
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock: WithLock callers did not finish")
	}

	if overlaps.Load() > 0 {
		t.Errorf("mutual exclusion violated: %d overlapping critical sections", overlaps.Load())
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("WithLock call %d returned %v; blocking contract requires waiting for the lock, not skipping fn", i, err)
		}
	}
	if got := ran.Load(); got != goroutines {
		t.Errorf("fn ran %d/%d times; blocking contract requires every contender to eventually run fn", got, goroutines)
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
