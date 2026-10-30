package coord

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recorder collects Apply and Full calls from a Watcher.
type recorder struct {
	mu    sync.Mutex
	apply [][]string
	full  [][]string
}

func strs(ms [][]byte) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = string(m)
	}
	return out
}

func (r *recorder) onApply(ms [][]byte) {
	r.mu.Lock()
	r.apply = append(r.apply, strs(ms))
	r.mu.Unlock()
}

func (r *recorder) onFull(ms [][]byte) {
	r.mu.Lock()
	r.full = append(r.full, strs(ms))
	r.mu.Unlock()
}

func (r *recorder) applied() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.apply)
}

func (r *recorder) fulls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.full)
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestWatcherListThenWatch(t *testing.T) {
	s := newFakeStore()
	s.zadd("{breakers}:changed", "k1", 10)
	s.zadd("{breakers}:changed", "k2", 11)
	n := NewNotifier(s, 16, nil)
	var r recorder
	w := NewWatcher(s, n, WatchOptions{
		Key:   "{breakers}:changed",
		Topic: "breakers",
		Every: 5 * time.Millisecond,
		Apply: r.onApply,
		Full:  r.onFull,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	eventually(t, "the first full list", func() bool { return len(r.fulls()) == 1 })
	if got := r.fulls()[0]; !slices.Equal(got, []string{"k1", "k2"}) {
		t.Fatalf("full list = %v", got)
	}

	// A push is applied at once.
	n.Publish("breakers", []byte("k1"))
	eventually(t, "the push", func() bool { return len(r.applied()) == 1 })
	if got := r.applied()[0]; !slices.Equal(got, []string{"k1"}) {
		t.Fatalf("push applied = %v", got)
	}

	// A change whose push was lost is picked up by the watermark re-list, and
	// members at or below the watermark are not listed again.
	s.zadd("{breakers}:changed", "k3", 12)
	eventually(t, "the re-list", func() bool { return len(r.applied()) == 2 })
	time.Sleep(20 * time.Millisecond)
	got := r.applied()
	if !slices.Equal(got[1], []string{"k3"}) || len(got) != 2 {
		t.Fatalf("applied = %v", got)
	}

	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("Run = %v", err)
	}
}

func TestWatcherRetriesFirstListAndSubscribe(t *testing.T) {
	s := newFakeStore()
	s.setDown(true)
	s.zadd("set", "m", 1)
	n := NewNotifier(s, 16, nil)
	var r recorder
	w := NewWatcher(s, n, WatchOptions{Key: "set", Topic: "topic", Every: 2 * time.Millisecond, Apply: r.onApply})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	if len(r.applied()) != 0 {
		t.Fatal("applied while the store was down")
	}
	s.setDown(false)
	eventually(t, "the full list after recovery", func() bool { return len(r.applied()) == 1 })
	eventually(t, "the late subscribe", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.subs["topic"]) == 1
	})
	n.Publish("topic", []byte("m"))
	eventually(t, "a push after a late subscribe", func() bool { return len(r.applied()) == 2 })
}

func TestNotifierDropsWhenFull(t *testing.T) {
	s := newFakeStore()
	gate := make(chan struct{})
	s.gate = gate
	var drops atomic.Int32
	n := NewNotifier(s, 2, func() { drops.Add(1) })
	for i := range 5 {
		n.Publish("t", []byte{byte('a' + i)})
	}
	// One message is held by the blocked sender, two wait in the queue.
	eventually(t, "drops", func() bool { return drops.Load() >= 2 })
	close(gate)
	eventually(t, "delivery", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.published)+int(drops.Load()) == 5
	})
	if d := drops.Load(); d < 2 || d > 3 {
		t.Fatalf("drops = %d", d)
	}

	before := drops.Load()
	s.setDown(true)
	n.Publish("t", []byte("x"))
	eventually(t, "a failed send counted", func() bool { return drops.Load() == before+1 })
}
