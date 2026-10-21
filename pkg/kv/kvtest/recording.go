package kvtest

import (
	"context"
	"sync"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// ScriptStore is a store that runs scripts singly and in batches, as kv.Mem
// and kv.Redis both do.
type ScriptStore interface {
	kv.Store
	kv.Scripter
	kv.BatchScripter
}

// Recording runs every call on the store it wraps and records each script,
// batched or not, and each round trip, so a test can pin what a request sent
// and what it cost. A store that only counted RunScript would read a batched
// commit as free.
type Recording struct {
	ScriptStore

	mu      sync.Mutex
	scripts []kv.ScriptCall
	batches []int
	trips   int
}

// NewRecording wraps s. Scripts the consumer expects must already be
// registered on s: consumers that register on a bare *kv.Mem do not see
// through the wrapper.
func NewRecording(s ScriptStore) *Recording { return &Recording{ScriptStore: s} }

func (r *Recording) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	r.trip()
	r.record(kv.ScriptCall{Name: name, Script: script, Keys: keys, Args: args})
	return r.ScriptStore.RunScript(ctx, name, script, keys, args...)
}

func (r *Recording) RunScriptBatch(ctx context.Context, calls []kv.ScriptCall) []kv.ScriptResult {
	r.batch(len(calls))
	for _, c := range calls {
		r.record(c)
	}
	return r.ScriptStore.RunScriptBatch(ctx, calls)
}

// ExecBatch implements kv.Batcher: one round trip, run by the wrapped store's
// own pipeline when it has one.
func (r *Recording) ExecBatch(ctx context.Context, ops []kv.BatchOp) {
	r.batch(len(ops))
	for _, op := range ops {
		if op.Kind == kv.OpScript {
			r.record(op.Script)
		}
	}
	if b, ok := r.ScriptStore.(kv.Batcher); ok {
		b.ExecBatch(ctx, ops)
		return
	}
	kv.ExecInOrder(ctx, r.ScriptStore, ops)
}

func (r *Recording) Get(ctx context.Context, key string) ([]byte, error) {
	r.trip()
	return r.ScriptStore.Get(ctx, key)
}

func (r *Recording) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	r.trip()
	return r.ScriptStore.Set(ctx, key, value, ttl)
}

func (r *Recording) Del(ctx context.Context, key string) error {
	r.trip()
	return r.ScriptStore.Del(ctx, key)
}

func (r *Recording) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	r.trip()
	return r.ScriptStore.Incr(ctx, key, delta)
}

func (r *Recording) Expire(ctx context.Context, key string, ttl time.Duration) error {
	r.trip()
	return r.ScriptStore.Expire(ctx, key, ttl)
}

func (r *Recording) Range(ctx context.Context, prefix string) ([]kv.Entry, error) {
	r.trip()
	return r.ScriptStore.Range(ctx, prefix)
}

func (r *Recording) WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error {
	r.trip()
	return r.ScriptStore.WithLock(ctx, keys, fn)
}

func (r *Recording) trip() {
	r.mu.Lock()
	r.trips++
	r.mu.Unlock()
}

func (r *Recording) batch(size int) {
	r.mu.Lock()
	r.trips++
	r.batches = append(r.batches, size)
	r.mu.Unlock()
}

func (r *Recording) record(c kv.ScriptCall) {
	c.Keys = append([]string(nil), c.Keys...)
	r.mu.Lock()
	r.scripts = append(r.scripts, c)
	r.mu.Unlock()
}

// Scripts returns every script run so far, in order, batched ones included.
func (r *Recording) Scripts() []kv.ScriptCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kv.ScriptCall(nil), r.scripts...)
}

// Named returns the scripts run so far under name, in order.
func (r *Recording) Named(name string) []kv.ScriptCall {
	var out []kv.ScriptCall
	for _, c := range r.Scripts() {
		if c.Name == name {
			out = append(out, c)
		}
	}
	return out
}

// Batches returns the number of commands each batch carried (RunScriptBatch
// or kv.Batch), one entry per batched round trip.
func (r *Recording) Batches() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.batches...)
}

// RoundTrips counts the store calls so far: one per command outside a batch
// and one per batch. Calls a script makes inside the store are not counted.
func (r *Recording) RoundTrips() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.trips
}
