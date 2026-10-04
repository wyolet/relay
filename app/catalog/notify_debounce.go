package catalog

import (
	"sync"
	"time"
)

// ── debouncer ─────────────────────────────────────────────────────────────────

type eventKey struct{ Kind, ID string }

const debounceCap = 1000

type debouncer struct {
	mu       sync.Mutex
	pending  map[eventKey]string // value = op; last-write-wins
	interval time.Duration
}

func newDebouncer(interval time.Duration) *debouncer {
	return &debouncer{
		pending:  make(map[eventKey]string, 64),
		interval: interval,
	}
}

// push records an event. Returns true if the buffer hit the soft cap and
// the caller should trigger an immediate flush.
func (d *debouncer) push(e notifyEvent) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending[eventKey{e.Kind, e.ID}] = e.Op
	return len(d.pending) >= debounceCap
}

// requeue puts back an event that failed to apply, unless a newer event for
// the same row arrived since the drain.
func (d *debouncer) requeue(e drainedEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	k := eventKey{e.Kind, e.ID}
	if _, newer := d.pending[k]; !newer {
		d.pending[k] = e.Op
	}
}

type drainedEvent struct {
	Kind string
	ID   string
	Op   string
}

// drain atomically extracts all pending events and returns them as a slice.
func (d *debouncer) drain() []drainedEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.pending) == 0 {
		return nil
	}
	out := make([]drainedEvent, 0, len(d.pending))
	for k, op := range d.pending {
		out = append(out, drainedEvent{Kind: k.Kind, ID: k.ID, Op: op})
	}
	d.pending = make(map[eventKey]string, 64)
	return out
}
