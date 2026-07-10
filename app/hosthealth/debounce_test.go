package hosthealth_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hosthealth"
	"github.com/wyolet/relay/pkg/kv"
)

// countingStore wraps a kv.Store and counts Set calls, so we can assert how
// many reachability writes actually reached kv.
type countingStore struct {
	inner kv.Store
	mu    sync.Mutex
	sets  int
}

func (c *countingStore) Get(ctx context.Context, k string) ([]byte, error) {
	return c.inner.Get(ctx, k)
}
func (c *countingStore) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	c.mu.Lock()
	c.sets++
	c.mu.Unlock()
	return c.inner.Set(ctx, k, v, ttl)
}
func (c *countingStore) Del(ctx context.Context, k string) error { return c.inner.Del(ctx, k) }
func (c *countingStore) Incr(ctx context.Context, k string, d int64) (int64, error) {
	return c.inner.Incr(ctx, k, d)
}
func (c *countingStore) Expire(ctx context.Context, k string, ttl time.Duration) error {
	return c.inner.Expire(ctx, k, ttl)
}
func (c *countingStore) Range(ctx context.Context, p string) ([]kv.Entry, error) {
	return c.inner.Range(ctx, p)
}
func (c *countingStore) WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error {
	return c.inner.WithLock(ctx, keys, fn)
}
func (c *countingStore) Close() error { return c.inner.Close() }

func (c *countingStore) setCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sets
}

func newRecorder(t *testing.T) (*hosthealth.Recorder, *countingStore, *time.Time) {
	t.Helper()
	cs := &countingStore{inner: kv.NewMem()}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nowp := &now
	clock := func() time.Time { return *nowp }
	return hosthealth.New(cs, clock), cs, nowp
}

// TestReachable_DebouncedPerHost: a burst of Reachable to one host writes once
// per debounce interval, not once per call.
func TestReachable_DebouncedPerHost(t *testing.T) {
	r, cs, now := newRecorder(t)
	ctx := context.Background()

	// First write always lands.
	r.Reachable(ctx, "h1")
	if cs.setCount() != 1 {
		t.Fatalf("after first Reachable: sets=%d, want 1", cs.setCount())
	}

	// A burst within the same instant is fully debounced.
	for i := 0; i < 1000; i++ {
		r.Reachable(ctx, "h1")
	}
	if cs.setCount() != 1 {
		t.Fatalf("after burst within interval: sets=%d, want 1", cs.setCount())
	}

	// Advancing past the interval allows exactly one more.
	*now = now.Add(1100 * time.Millisecond)
	r.Reachable(ctx, "h1")
	r.Reachable(ctx, "h1")
	if cs.setCount() != 2 {
		t.Fatalf("after interval elapsed: sets=%d, want 2", cs.setCount())
	}

	// A different host is debounced independently (its own first write lands).
	r.Reachable(ctx, "h2")
	if cs.setCount() != 3 {
		t.Fatalf("distinct host: sets=%d, want 3", cs.setCount())
	}
}

// TestUnreachable_ImmediateAndResetsDebounce: failure writes are never
// debounced, and they clear the gate so the next Reachable (recovery) writes at
// once even within the interval.
func TestUnreachable_ImmediateAndResetsDebounce(t *testing.T) {
	r, cs, _ := newRecorder(t)
	ctx := context.Background()

	r.Reachable(ctx, "h1") // 1
	// Two failures in the same instant both write (no debounce on failure).
	r.Unreachable(ctx, "h1", "dial tcp: refused") // 2
	r.Unreachable(ctx, "h1", "dial tcp: refused") // 3
	if cs.setCount() != 3 {
		t.Fatalf("failures must not be debounced: sets=%d, want 3", cs.setCount())
	}
	if st, _ := r.Read(ctx, "h1"); st.Health != host.HealthUnreachable || st.ConsecutiveFailures != 2 {
		t.Fatalf("health=%q fails=%d, want unreachable/2", st.Health, st.ConsecutiveFailures)
	}

	// Recovery writes immediately despite being within the debounce interval,
	// because Unreachable cleared the gate.
	r.Reachable(ctx, "h1") // 4
	if cs.setCount() != 4 {
		t.Fatalf("recovery must write immediately: sets=%d, want 4", cs.setCount())
	}
	if st, _ := r.Read(ctx, "h1"); st.Health != host.HealthHealthy || st.ConsecutiveFailures != 0 {
		t.Fatalf("after recovery: health=%q fails=%d, want healthy/0", st.Health, st.ConsecutiveFailures)
	}
}
