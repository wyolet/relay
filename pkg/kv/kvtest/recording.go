package kvtest

import (
	"context"
	"sync"

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
// batched or not, so a test can pin what a request sent and how many round
// trips it cost. A store that only counted RunScript would read a batched
// commit as free.
type Recording struct {
	ScriptStore

	mu      sync.Mutex
	scripts []kv.ScriptCall
	batches []int
}

// NewRecording wraps s. Scripts the consumer expects must already be
// registered on s: consumers that register on a bare *kv.Mem do not see
// through the wrapper.
func NewRecording(s ScriptStore) *Recording { return &Recording{ScriptStore: s} }

func (r *Recording) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	r.record(kv.ScriptCall{Name: name, Script: script, Keys: keys, Args: args})
	return r.ScriptStore.RunScript(ctx, name, script, keys, args...)
}

func (r *Recording) RunScriptBatch(ctx context.Context, calls []kv.ScriptCall) []kv.ScriptResult {
	r.mu.Lock()
	r.batches = append(r.batches, len(calls))
	r.mu.Unlock()
	for _, c := range calls {
		r.record(c)
	}
	return r.ScriptStore.RunScriptBatch(ctx, calls)
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

// Batches returns the number of scripts each RunScriptBatch carried, one
// entry per batched round trip.
func (r *Recording) Batches() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.batches...)
}
