package kv_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// withLockBlockingContract asserts the blocking WithLock contract the
// kv.Store interface documents: under contention a WithLock call WAITS
// for the lock and then runs fn — it never skips fn or surfaces a busy
// error. Mutual exclusion must hold while it does so.
//
// Audit 2026-07-04 (P1 tracker #9): kv.Redis.WithLock used to be
// non-blocking (ErrLockBusy without running fn on contention), diverging
// from kv.Mem. This contract test runs against both backends (Redis leg
// lives in withlock_contract_integration_test.go, gated the same way as
// the rest of the Redis contract suite) and fails on any backend that
// violates the blocking contract.
func withLockBlockingContract(t *testing.T, s kv.Store) {
	t.Helper()
	ctx := context.Background()

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
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = s.WithLock(ctx, []string{"{wlc}:lock"}, func(context.Context) error {
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
	wg.Wait()

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

func TestWithLockBlockingContract_Mem(t *testing.T) {
	s := kv.NewMem()
	t.Cleanup(func() { _ = s.Close() })
	withLockBlockingContract(t, s)
}
