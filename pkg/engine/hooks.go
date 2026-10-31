package engine

import "sync"

// hookQueue runs Hooks calls outside the engine lock, one at a time and in the order they were queued, so a hook may write to the engine.
type hookQueue struct {
	mu      sync.Mutex
	idle    sync.Cond // broadcast after each call finishes
	calls   []func()
	queued  uint64 // calls ever queued
	done    uint64 // calls finished
	running bool   // a goroutine is running calls
}

func (q *hookQueue) init() { q.idle.L = &q.mu }

// add queues one call; the engine lock is held, so calls queue in event order.
func (q *hookQueue) add(f func()) {
	q.mu.Lock()
	q.calls = append(q.calls, f)
	q.queued++
	q.mu.Unlock()
}

// run calls the queued hooks on this goroutine. When another goroutine is already running them it returns at once and that goroutine runs these too: waiting here would deadlock a hook that writes to the engine.
func (q *hookQueue) run() {
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	for len(q.calls) > 0 {
		f := q.calls[0]
		q.calls[0] = nil
		q.calls = q.calls[1:]
		q.mu.Unlock()
		q.call(f)
		q.mu.Lock()
		q.done++
		q.idle.Broadcast()
	}
	q.calls = nil
	q.running = false
	q.mu.Unlock()
}

// call runs one hook; a panicking hook releases the queue before the panic propagates, so the next run picks up the rest.
func (q *hookQueue) call(f func()) {
	ok := false
	defer func() {
		if !ok {
			q.mu.Lock()
			q.done++
			q.running = false
			q.idle.Broadcast()
			q.mu.Unlock()
		}
	}()
	f()
	ok = true
}

// flush runs the queued hooks and waits until every call queued before it has finished. Never from a hook.
func (q *hookQueue) flush() {
	q.mu.Lock()
	target := q.queued
	q.mu.Unlock()
	q.run()
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.done < target {
		if !q.running {
			// The previous runner panicked and left calls behind.
			q.mu.Unlock()
			q.run()
			q.mu.Lock()
			continue
		}
		q.idle.Wait()
	}
}
