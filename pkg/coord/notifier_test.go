package coord_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/coord"
)

// inbox collects delivered payloads.
type inbox struct {
	mu  sync.Mutex
	got []string
}

func (b *inbox) add(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, string(p))
}

func (b *inbox) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.got)
}

func subscribe(t *testing.T, s *memStore, n *coord.Notifier, topic string) *inbox {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := &inbox{}
	before := s.subscribers(topic)
	go func() { _ = n.Subscribe(ctx, topic, b.add) }()
	waitUntil(t, "subscription", func() bool { return s.subscribers(topic) > before })
	return b
}

func TestNotifierDeliversPublishedPayloads(t *testing.T) {
	s := newMemStore()
	n := coord.NewNotifier(s, 8, time.Second, nil)
	b := subscribe(t, s, n, "breakers")
	n.Publish("breakers", []byte("k1"))
	n.Publish("other", []byte("k2"))
	waitUntil(t, "delivery", func() bool { return b.len() == 1 })
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.got[0] != "k1" {
		t.Fatalf("delivered %v, want [k1]", b.got)
	}
}

func TestNotifierSubscriptionEndsWithContext(t *testing.T) {
	s := newMemStore()
	n := coord.NewNotifier(s, 8, time.Second, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = n.Subscribe(ctx, "t", func([]byte) {})
	}()
	waitUntil(t, "subscription", func() bool { return s.subscribers("t") == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe did not return after its context ended")
	}
	waitUntil(t, "unsubscribe", func() bool { return s.subscribers("t") == 0 })
}

func TestPublishNeverBlocksAndDropsWhenQueueIsFull(t *testing.T) {
	s := newMemStore()
	release := make(chan struct{})
	s.pubBlock = release
	var drops atomic.Int32
	n := coord.NewNotifier(s, 2, 10*time.Second, func() { drops.Add(1) })
	b := subscribe(t, s, n, "t")

	start := time.Now()
	for range 10 {
		n.Publish("t", []byte("m"))
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("10 publishes against a hung store took %v", d)
	}
	waitUntil(t, "drops", func() bool { return drops.Load() >= 7 })
	if d := drops.Load(); d > 8 {
		t.Fatalf("dropped %d of 10 with a queue of 2, want 7 or 8", d)
	}

	close(release)
	waitUntil(t, "queued messages to deliver", func() bool { return int(drops.Load())+b.len() == 10 })
}

func TestTimedOutSendCountsAsDrop(t *testing.T) {
	s := newMemStore()
	s.pubBlock = make(chan struct{})
	var drops atomic.Int32
	n := coord.NewNotifier(s, 4, 20*time.Millisecond, func() { drops.Add(1) })

	start := time.Now()
	n.Publish("t", []byte("m"))
	waitUntil(t, "timeout drop", func() bool { return drops.Load() == 1 })
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("a 20ms send timeout dropped after %v", d)
	}
}

func TestZeroTimeoutBoundsSendsAtOneSecond(t *testing.T) {
	s := newMemStore()
	s.pubBlock = make(chan struct{})
	var drops atomic.Int32
	n := coord.NewNotifier(s, 4, 0, func() { drops.Add(1) })

	start := time.Now()
	n.Publish("t", []byte("m"))
	waitUntil(t, "timeout drop", func() bool { return drops.Load() == 1 })
	if d := time.Since(start); d < 900*time.Millisecond || d > 2500*time.Millisecond {
		t.Fatalf("default send timeout dropped after %v, want about 1s", d)
	}
}

func TestFailedSendCountsAsDrop(t *testing.T) {
	s := newMemStore()
	s.pubErr = errors.New("publish refused")
	var drops atomic.Int32
	n := coord.NewNotifier(s, 4, time.Second, func() { drops.Add(1) })
	n.Publish("t", []byte("a"))
	n.Publish("t", []byte("b"))
	waitUntil(t, "failed-send drops", func() bool { return drops.Load() == 2 })
}

func TestHungSendDoesNotStallLaterMessages(t *testing.T) {
	s := newMemStore()
	release := make(chan struct{})
	s.pubBlock = release
	var drops atomic.Int32
	n := coord.NewNotifier(s, 4, 30*time.Millisecond, func() { drops.Add(1) })
	b := subscribe(t, s, n, "t")

	n.Publish("t", []byte("lost"))
	waitUntil(t, "first drop", func() bool { return drops.Load() == 1 })
	s.mu.Lock()
	s.pubBlock = nil
	s.mu.Unlock()
	close(release)
	n.Publish("t", []byte("kept"))
	waitUntil(t, "later delivery", func() bool { return b.len() == 1 })
}
