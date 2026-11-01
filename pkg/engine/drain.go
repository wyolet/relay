package engine

import (
	"context"
	"time"
)

// loop is the drain goroutine: it waits for events or a retry deadline,
// debounces, drains and publishes.
func (e *Engine) loop() {
	defer close(e.loopDone)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-e.closing:
			return
		case <-e.wake:
			if !e.debounce() {
				return
			}
		case <-timer.C:
		}
		e.mu.Lock()
		e.drainLocked(e.ctx)
		if e.resyncDue {
			e.resyncDue = false
			if err := e.resyncLocked(e.ctx); err != nil {
				e.resyncDue = true
			}
		}
		next := e.nextWake()
		e.mu.Unlock()
		e.hooks.run()
		timer.Stop()
		if !next.IsZero() {
			timer.Reset(max(time.Until(next), 0))
		}
	}
}

func (e *Engine) debounce() bool {
	if e.opts.Debounce <= 0 {
		return true
	}
	t := time.NewTimer(e.opts.Debounce)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-e.closing:
		return false
	}
}

// drainLocked applies the queued events and the retries now due as one batch.
func (e *Engine) drainLocked(ctx context.Context) {
	if e.stopped {
		return
	}
	e.qmu.Lock()
	evs := make([]Event, 0, len(e.queue))
	for _, ev := range e.queue {
		evs = append(evs, ev)
	}
	clear(e.queue)
	e.qmu.Unlock()

	e.fetchWake = time.Time{}
	now := time.Now()
	b := e.newBatch()
	for _, ev := range evs {
		b.applyEvent(ctx, ev)
	}
	for ref, at := range e.retries {
		if !at.After(now) {
			b.dirty(e.sch.byKind[ref.Kind].producer, ref.ID)
		}
	}
	b.settle()
	b.publish(false)
	if b.abandoned {
		e.resyncDue = true
	}
}

// nextWake is the earliest time the loop must drain without a new event.
func (e *Engine) nextWake() time.Time {
	next := e.fetchWake
	for _, at := range e.retries {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	if e.resyncDue {
		if at := time.Now().Add(maxBackoff); next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}

func (e *Engine) retryBase() time.Duration {
	if e.opts.Debounce > 0 {
		return e.opts.Debounce
	}
	return minRetry
}

func (b *batch) applyEvent(ctx context.Context, ev Event) {
	e := b.e
	k := e.sch.byKind[ev.Ref.Kind]
	if k == nil || !k.input() {
		return
	}
	id := ev.Ref.ID
	if ev.Op == OpDelete {
		if b.write(k, id, nil) {
			b.stats.Inputs++
		}
		return
	}
	if cur, had := e.st.objs[k.idx].get(id); had && stale(cur.meta.Version, ev.Version) {
		return
	}
	row, err := k.get(ctx, id)
	if err != nil {
		e.qmu.Lock()
		if _, newer := e.queue[ev.Ref]; !newer {
			e.queue[ev.Ref] = ev
		}
		e.qmu.Unlock()
		e.fetchWake = time.Now().Add(e.retryBase())
		return
	}
	b.applyRow(k, id, row)
}

// applyRow writes an input row (nil deletes) unless the held row is newer.
func (b *batch) applyRow(k *kindInfo, id string, row any) {
	if row != nil {
		if cur, had := b.e.st.objs[k.idx].get(id); had && stale(cur.meta.Version, k.meta(row).Version) {
			return
		}
	}
	if b.write(k, id, row) {
		b.stats.Inputs++
	}
}

// stale reports whether a row at version got adds nothing to one at held.
func stale(held, got int64) bool { return held != 0 && got != 0 && got <= held }
