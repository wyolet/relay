package otlpreceiver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// markerTTL is how long a call stays recognisable as already stored. It has to outlast an exporter's retries, not the data.
const markerTTL = 24 * time.Hour

const (
	scriptMark   = "otlpreceiver.mark"
	scriptUnmark = "otlpreceiver.unmark"
)

// markScript sets every key that is absent and returns one character per key: '1' when this call set it, '0' when it was already there.
const markScript = `
local out = {}
for i, key in ipairs(KEYS) do
	if redis.call('SET', key, '1', 'NX', 'PX', ARGV[1]) then
		out[i] = '1'
	else
		out[i] = '0'
	end
end
return table.concat(out)
`

// unmarkScript deletes key by key: unpack(KEYS) overflows the Lua stack on a large export.
const unmarkScript = `
for _, key in ipairs(KEYS) do
	redis.call('DEL', key)
end
return 1
`

// Call identifies one reported model call within a tenant. Every signal that reports the same call uses the same identity: the ids of the span the call ran in. A call reported without them is identified by a digest of its response id in TraceID.
type Call struct {
	TraceID string
	SpanID  string
}

// RequestID is the request id of the call's usage event and of its stored content. Derived from the identity, so a call the client sends twice keeps one id.
func (c Call) RequestID() string { return "otlp-" + c.TraceID + "-" + c.SpanID }

// Markers remembers which reported calls were already stored, so a call an exporter sends again is stored once. Every method takes the tenant the calls were reported for: the same ids reported by two tenants are two calls. Safe for concurrent use.
type Markers struct {
	runner kv.Scripter
	batch  kv.BatchScripter
}

// NewMarkers returns Markers over s, which may also implement kv.BatchScripter to mark an export in one round trip.
func NewMarkers(s kv.Scripter) *Markers {
	m := &Markers{runner: s}
	m.batch, _ = s.(kv.BatchScripter)
	if mem, ok := s.(*kv.Mem); ok {
		mem.RegisterScript(scriptMark, memMark)
		mem.RegisterScript(scriptUnmark, memUnmark)
	}
	return m
}

// Mark marks kind as stored for each call and reports, per call, whether this was the first time. calls must not repeat a call. When the store fails, the calls it could not answer for are reported as first-time and the error is returned, so a failing store never loses a record.
func (m *Markers) Mark(ctx context.Context, tenant string, kind MarkerKind, calls []Call) ([]bool, error) {
	fresh := make([]bool, len(calls))
	groups := groupByTrace(calls)
	results := m.run(ctx, scriptMark, markScript, tenant, kind, calls, groups, markerTTL.Milliseconds())
	var errs []error
	for g, members := range groups {
		res := results[g]
		if res.Err == nil && len(res.Value) != len(members) {
			res.Err = fmt.Errorf("otlpreceiver: mark returned %d results for %d keys", len(res.Value), len(members))
		}
		if res.Err != nil {
			errs = append(errs, res.Err)
		}
		for i, idx := range members {
			fresh[idx] = res.Err != nil || res.Value[i] == '1'
		}
	}
	return fresh, errors.Join(errs...)
}

// Unmark removes the kind marker of each call, for calls that were marked and then could not be stored.
func (m *Markers) Unmark(ctx context.Context, tenant string, kind MarkerKind, calls []Call) error {
	var errs []error
	for _, res := range m.run(ctx, scriptUnmark, unmarkScript, tenant, kind, calls, groupByTrace(calls)) {
		if res.Err != nil {
			errs = append(errs, res.Err)
		}
	}
	return errors.Join(errs...)
}

// groupByTrace splits calls into one group of indexes per trace id, because the keys of one script call must share a hash tag.
func groupByTrace(calls []Call) [][]int {
	var groups [][]int
	byTrace := make(map[string]int)
	for i, c := range calls {
		g, ok := byTrace[c.TraceID]
		if !ok {
			g = len(groups)
			byTrace[c.TraceID] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}
	return groups
}

func (m *Markers) run(ctx context.Context, name, script, tenant string, kind MarkerKind, calls []Call, groups [][]int, args ...any) []kv.ScriptResult {
	scriptCalls := make([]kv.ScriptCall, len(groups))
	for g, members := range groups {
		keys := make([]string, len(members))
		for i, idx := range members {
			keys[i] = markerKey(tenant, kind, calls[idx])
		}
		scriptCalls[g] = kv.ScriptCall{Name: name, Script: script, Keys: keys, Args: args}
	}
	if m.batch != nil {
		return m.batch.RunScriptBatch(ctx, scriptCalls)
	}
	results := make([]kv.ScriptResult, len(scriptCalls))
	for i, c := range scriptCalls {
		results[i].Value, results[i].Err = m.runner.RunScript(ctx, c.Name, c.Script, c.Keys, c.Args...)
	}
	return results
}

func memMark(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("%s: expected 1 arg, got %d", scriptMark, len(args))
	}
	ttlMs, ok := args[0].(int64)
	if !ok {
		return nil, fmt.Errorf("%s: ttl must be int64, got %T", scriptMark, args[0])
	}
	out := make([]byte, len(keys))
	// One lock for every mark rather than one per key: the in-memory store keeps a mutex for each key it ever locked.
	err := store.WithLock(ctx, []string{memMarkLock}, func(ctx context.Context) error {
		for i, key := range keys {
			if _, err := store.Get(ctx, key); err == nil {
				out[i] = '0'
				continue
			}
			if err := store.Set(ctx, key, []byte("1"), time.Duration(ttlMs)*time.Millisecond); err != nil {
				return err
			}
			out[i] = '1'
		}
		return nil
	})
	return out, err
}

func memUnmark(ctx context.Context, store *kv.Mem, keys []string, _ []any) ([]byte, error) {
	for _, key := range keys {
		if err := store.Del(ctx, key); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
