package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

// Apply writes an input row the caller already holds and drains at once, so
// a write is visible on the writing process before its notification returns.
func Apply[T any](e *Engine, row *T) error {
	k := e.sch.byType[reflect.TypeFor[T]()]
	switch {
	case row == nil:
		return errors.New("engine: Apply of a nil row")
	case k == nil || !k.input():
		return fmt.Errorf("engine: Apply: %v is not an input kind", reflect.TypeFor[T]())
	}
	id := k.meta(row).ID
	if id == "" {
		return fmt.Errorf("engine: Apply: %s row without an id", k.kind)
	}
	return e.writeNow(k, id, row)
}

// Delete removes an input row and drains at once; Apply's counterpart.
func Delete(e *Engine, ref Ref) error {
	k := e.sch.byKind[ref.Kind]
	if k == nil || !k.input() {
		return fmt.Errorf("engine: Delete: %q is not an input kind", ref.Kind)
	}
	return e.writeNow(k, ref.ID, nil)
}

func (e *Engine) writeNow(k *kindInfo, id string, row any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return err
	}
	b := e.newBatch()
	b.applyRow(k, id, row)
	b.settle()
	b.publish(false)
	e.afterBatch(b)
	return nil
}

func (e *Engine) usable() error {
	switch {
	case e.stopped:
		return errClosed
	case !e.loaded.Load():
		return errNotLoaded
	}
	return nil
}

// afterBatch hands follow-up work from a synchronous batch to the loop.
func (e *Engine) afterBatch(b *batch) {
	if b.abandoned {
		e.resyncDue = true
	}
	if e.resyncDue || len(e.retries) > 0 {
		e.kick()
	}
}

// Resync lists every input kind, applies the differences against the held
// versions (or values, when a Version is 0) and publishes only when something
// changed.
func (e *Engine) Resync(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.usable(); err != nil {
		return err
	}
	return e.resyncLocked(ctx)
}

func (e *Engine) resyncLocked(ctx context.Context) error {
	start := time.Now()
	lists := make([][]any, len(e.sch.kinds))
	for i, k := range e.sch.kinds {
		if !k.input() {
			continue
		}
		rows, err := k.list(ctx)
		if err != nil {
			return fmt.Errorf("engine: list %s: %w", k.kind, err)
		}
		lists[i] = rows
	}
	b := e.newBatch()
	for i, k := range e.sch.kinds {
		if !k.input() {
			continue
		}
		seen := make(map[string]struct{}, len(lists[i]))
		for _, row := range lists[i] {
			m := k.meta(row)
			if m.ID == "" {
				continue
			}
			seen[m.ID] = struct{}{}
			if cur, had := e.st.objs[i].get(m.ID); had && !differs(cur, m, row) {
				continue
			}
			if b.write(k, m.ID, row) {
				b.stats.Inputs++
			}
		}
		for _, id := range e.st.objs[i].cur.keys() {
			if _, ok := seen[id]; !ok && b.write(k, id, nil) {
				b.stats.Inputs++
			}
		}
	}
	b.settle()
	b.publish(false)
	e.afterBatch(b)
	if h := e.opts.Hooks.OnResync; h != nil {
		h(b.stats.Inputs, time.Since(start))
	}
	return nil
}

func differs(cur obj, m Meta, row any) bool {
	if cur.meta.Version != 0 && m.Version != 0 {
		return cur.meta.Version != m.Version
	}
	return !reflect.DeepEqual(cur.v, row)
}
