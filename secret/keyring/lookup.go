package keyring

import (
	"context"
	"errors"
	"fmt"
)

var errReadPanicked = errors.New("keyring: data key read panicked")

type flight struct {
	done chan struct{}
	err  error
}

// ready reports whether an entry can be answered without a store read: a held DEK or a tombstone. Unknown and locked keys are read again, so a key created or rewrapped elsewhere opens without waiting for a notification.
func ready(e *entry) bool {
	return e != nil && (e.dek != nil || e.key.State == Destroyed)
}

// lookup returns the key for id, reading it from the store once when this Ring has not seen it or cannot unwrap it. Concurrent reads for one id share a flight, and an id that is still unknown or locked afterwards is not read again for Options.UnknownKeyTTL.
func (r *Ring) lookup(ctx context.Context, id [16]byte) (*view, *entry, error) {
	if v := r.view.Load(); ready(v.keys[id]) {
		return v, v.keys[id], nil
	}
	r.flightMu.Lock()
	if v := r.view.Load(); ready(v.keys[id]) {
		r.flightMu.Unlock()
		return v, v.keys[id], nil
	}
	if exp, ok := r.misses[id]; ok && r.opts.Now().Before(exp) {
		r.flightMu.Unlock()
		return found(r.view.Load(), id)
	}
	f := r.flights[id]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		r.flights[id] = f
		r.flightMu.Unlock()
		r.lead(ctx, id, f)
	} else {
		r.flightMu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, nil, f.err
	}
	return found(r.view.Load(), id)
}

// lead reads id for flight f. The read ignores the leader's cancellation because waiters with live contexts share its result, and the deferred close releases them even when the read panics.
func (r *Ring) lead(ctx context.Context, id [16]byte, f *flight) {
	f.err = errReadPanicked
	defer func() {
		r.flightMu.Lock()
		if f.err == nil && !ready(r.view.Load().keys[id]) {
			r.rememberMissLocked(id)
		}
		delete(r.flights, id)
		close(f.done)
		r.flightMu.Unlock()
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	f.err = r.reloadIDsLocked(context.WithoutCancel(ctx), nil, id)
}

// found returns id's entry from v, locked ones included; the caller reports those.
func found(v *view, id [16]byte) (*view, *entry, error) {
	if e := v.keys[id]; e != nil {
		return v, e, nil
	}
	return nil, nil, fmt.Errorf("%w: %s", ErrUnknownKey, FormatID(id))
}

// rememberMissLocked records an id that is still unknown or locked and drops expired ones; the map stays bounded by the store reads made within one TTL. Caller holds r.flightMu.
func (r *Ring) rememberMissLocked(id [16]byte) {
	now := r.opts.Now()
	for k, exp := range r.misses {
		if !now.Before(exp) {
			delete(r.misses, k)
		}
	}
	r.misses[id] = now.Add(r.opts.UnknownKeyTTL)
}
