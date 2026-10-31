package coord_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/coord"
)

const changedKey = "{breakers}:changed"

// applyLog records Apply and Full calls with the members each received.
type applyLog struct {
	mu    sync.Mutex
	full  [][]string
	apply [][]string
}

func members(ms [][]byte) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, string(m))
	}
	return out
}

func (l *applyLog) onFull(ms [][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.full = append(l.full, members(ms))
}

func (l *applyLog) onApply(ms [][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.apply = append(l.apply, members(ms))
}

func (l *applyLog) fulls() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.full)
}

// applied returns every non-empty Apply call.
func (l *applyLog) applied() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out [][]string
	for _, a := range l.apply {
		if len(a) > 0 {
			out = append(out, a)
		}
	}
	return out
}

func appliedHas(l *applyLog, member string) bool {
	for _, a := range l.applied() {
		if slices.Contains(a, member) {
			return true
		}
	}
	return false
}

func runWatcher(t *testing.T, w *coord.Watcher) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("Watcher.Run did not return after its context ended")
			return nil
		}
	}
	t.Cleanup(func() { _ = stop() })
	return stop
}

func TestWatcherListsWholeSetFirst(t *testing.T) {
	s := newMemStore()
	s.zadd(changedKey, "b", 2)
	s.zadd(changedKey, "a", 1)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, nil, coord.WatchOptions{Key: changedKey, Every: time.Hour, Apply: l.onApply, Full: l.onFull}))
	waitUntil(t, "first list", func() bool { return len(l.fulls()) == 1 })
	if got := l.fulls()[0]; !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("first list = %v, want [a b]", got)
	}
}

func TestWatcherWithoutFullSendsFirstListToApply(t *testing.T) {
	s := newMemStore()
	s.zadd(changedKey, "a", 1)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, nil, coord.WatchOptions{Key: changedKey, Every: time.Hour, Apply: l.onApply}))
	waitUntil(t, "first list through Apply", func() bool { return appliedHas(l, "a") })
}

func TestWatcherAppliesPushAtOnce(t *testing.T) {
	s := newMemStore()
	s.zadd(changedKey, "a", 1)
	n := coord.NewNotifier(s, 8, time.Second, nil)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, n, coord.WatchOptions{Key: changedKey, Topic: "breakers", Every: time.Hour, Apply: l.onApply, Full: l.onFull}))
	waitUntil(t, "first list", func() bool { return len(l.fulls()) == 1 })
	waitUntil(t, "subscription", func() bool { return s.subscribers("breakers") > 0 })

	n.Publish("breakers", []byte("c"))
	waitUntil(t, "pushed member", func() bool { return appliedHas(l, "c") })
}

func TestWatcherSubscribesBeforeFirstList(t *testing.T) {
	s := newMemStore()
	n := coord.NewNotifier(s, 8, time.Second, nil)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, n, coord.WatchOptions{Key: changedKey, Topic: "breakers", Every: time.Hour, Apply: l.onApply, Full: l.onFull}))
	waitUntil(t, "first list", func() bool { return len(l.fulls()) == 1 })
	ev := s.eventLog()
	sub, list := slices.Index(ev, "subscribe:breakers"), slices.Index(ev, "pipeline")
	if sub < 0 || list < 0 || sub > list {
		t.Fatalf("store calls %v: want the subscribe before the first list", ev)
	}
}

func TestWatcherRelistsOnlyMembersAboveWatermark(t *testing.T) {
	s := newMemStore()
	s.zadd(changedKey, "a", 1)
	s.zadd(changedKey, "b", 2)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, nil, coord.WatchOptions{Key: changedKey, Every: 10 * time.Millisecond, Apply: l.onApply, Full: l.onFull}))
	waitUntil(t, "first list", func() bool { return len(l.fulls()) == 1 })

	s.zadd(changedKey, "c", 3)
	waitUntil(t, "unpushed change", func() bool { return appliedHas(l, "c") })
	s.zadd(changedKey, "a", 4)
	waitUntil(t, "second unpushed change", func() bool { return appliedHas(l, "a") })

	for _, a := range l.applied() {
		if !slices.Equal(a, []string{"c"}) && !slices.Equal(a, []string{"a"}) {
			t.Fatalf("re-list applied %v; want only members above the watermark", a)
		}
	}
	if n := len(l.fulls()); n != 1 {
		t.Fatalf("Full ran %d times with FullEvery unset, want 1", n)
	}
}

func TestWatcherRelistsWholeSetEveryFullEvery(t *testing.T) {
	s := newMemStore()
	s.zadd(changedKey, "a", 1)
	s.zadd(changedKey, "b", 2)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, nil, coord.WatchOptions{Key: changedKey, Every: 10 * time.Millisecond, FullEvery: 30 * time.Millisecond, Apply: l.onApply, Full: l.onFull}))
	waitUntil(t, "a second full list", func() bool { return len(l.fulls()) >= 2 })
	for _, f := range l.fulls() {
		if !slices.Equal(f, []string{"a", "b"}) {
			t.Fatalf("full list = %v, want [a b]", f)
		}
	}
}

func TestWatcherListsOnceStoreRecovers(t *testing.T) {
	s := newMemStore()
	s.zadd(changedKey, "a", 1)
	s.setFailing(true)
	l := &applyLog{}
	runWatcher(t, coord.NewWatcher(s, nil, coord.WatchOptions{Key: changedKey, Every: 10 * time.Millisecond, Apply: l.onApply, Full: l.onFull}))
	waitUntil(t, "a failed list attempt", func() bool { return slices.Contains(s.eventLog(), "pipeline") })
	if len(l.fulls()) != 0 || len(l.applied()) != 0 {
		t.Fatal("Watcher applied members from a failed list")
	}
	s.setFailing(false)
	waitUntil(t, "list after recovery", func() bool {
		return len(l.fulls()) > 0 || appliedHas(l, "a")
	})
}

func TestWatcherRunReturnsWhenContextEnds(t *testing.T) {
	s := newMemStore()
	stop := runWatcher(t, coord.NewWatcher(s, nil, coord.WatchOptions{Key: changedKey, Every: 10 * time.Millisecond, Apply: func([][]byte) {}}))
	time.Sleep(20 * time.Millisecond)
	_ = stop()
}
