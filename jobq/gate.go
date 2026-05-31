package jobq

import (
	"context"
	"sync"
)

// gate is a resizable counting semaphore. It bounds how many jobs run at once
// and supports live resizing: raising the limit immediately admits waiters;
// lowering it stops new admissions while letting in-flight work drain (running
// jobs are never killed to shrink the gate).
//
// Waking waiters uses a close-and-replace channel broadcast rather than
// sync.Cond, so acquire can also select on a caller's context cancellation.
type gate struct {
	mu     sync.Mutex
	notify chan struct{}
	limit  int
	inUse  int
}

func newGate(limit int) *gate {
	if limit < 1 {
		limit = 1
	}
	return &gate{limit: limit, notify: make(chan struct{})}
}

// acquire blocks until a slot is free or ctx is done. On success the caller
// must later call release exactly once.
func (g *gate) acquire(ctx context.Context) error {
	for {
		g.mu.Lock()
		if g.inUse < g.limit {
			g.inUse++
			g.mu.Unlock()
			return nil
		}
		ch := g.notify
		g.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			// woken by a release/resize; loop and re-check.
		}
	}
}

func (g *gate) release() {
	g.mu.Lock()
	if g.inUse > 0 {
		g.inUse--
	}
	g.wake()
	g.mu.Unlock()
}

// resize sets a new limit. Raising it wakes waiters; lowering it takes effect
// for future acquires (in-flight jobs drain naturally). limit < 1 is clamped.
func (g *gate) resize(limit int) {
	if limit < 1 {
		limit = 1
	}
	g.mu.Lock()
	g.limit = limit
	g.wake()
	g.mu.Unlock()
}

// available reports how many slots could be acquired right now (0 when the gate
// is full or over-subscribed after a shrink).
func (g *gate) available() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limit > g.inUse {
		return g.limit - g.inUse
	}
	return 0
}

// stats returns the current in-use count and limit. For metrics/tests.
func (g *gate) stats() (inUse, limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inUse, g.limit
}

// wake broadcasts to all current waiters. Caller must hold g.mu.
func (g *gate) wake() {
	close(g.notify)
	g.notify = make(chan struct{})
}
