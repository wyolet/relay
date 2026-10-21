package kv

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// runLua is an internal helper; it is NOT the exported RunScript.
// It does SCRIPT LOAD → EVALSHA with EVAL fallback.
func (r *Redis) runLua(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	sha, err := r.loadSHA(ctx, name, script)
	if err != nil {
		return nil, err
	}
	result, err := r.client.EvalSha(ctx, sha, keys, args...).Result()
	if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
		// Reload and retry.
		r.shas.Delete(name)
		sha, err = r.loadSHA(ctx, name, script)
		if err != nil {
			return nil, err
		}
		result, err = r.client.EvalSha(ctx, sha, keys, args...).Result()
		if err != nil {
			// Final fallback: plain EVAL.
			result, err = r.client.Eval(ctx, script, keys, args...).Result()
		}
	}
	if err != nil {
		return nil, err
	}
	return redisResultToBytes(result)
}

func (r *Redis) loadSHA(ctx context.Context, name, script string) (string, error) {
	if v, ok := r.shas.Load(name); ok {
		return v.(string), nil
	}
	sha, err := r.client.ScriptLoad(ctx, script).Result()
	if err != nil {
		return "", fmt.Errorf("state: SCRIPT LOAD %q: %w", name, err)
	}
	actual, _ := r.shas.LoadOrStore(name, sha)
	return actual.(string), nil
}

// RunScript implements Scripter.
func (r *Redis) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	r.inflight.Add(1)
	defer r.inflight.Done()
	return r.runLua(ctx, name, script, keys, args...)
}

// RunScriptBatch implements BatchScripter: it issues every call in a single
// pipeline (one network round trip on single-node; one per involved node on
// Cluster, dispatched together). Each call is an independent EVALSHA — the
// batch is NOT a transaction, so a per-call failure does not roll back the
// others. Keys within one call must share a hash tag; keys ACROSS calls may
// differ (that is the point — it batches away sequential round trips for
// operations that cannot share a CROSSSLOT-safe script).
func (r *Redis) RunScriptBatch(ctx context.Context, calls []ScriptCall) []ScriptResult {
	ops := make([]BatchOp, len(calls))
	outs := make([]BatchResult, len(calls))
	for i, c := range calls {
		ops[i] = BatchOp{Kind: OpScript, Script: c, Result: &outs[i]}
	}
	r.ExecBatch(ctx, ops)
	results := make([]ScriptResult, len(calls))
	for i, o := range outs {
		results[i] = ScriptResult{Value: o.Value, Err: o.Err}
	}
	return results
}

// redisResultToBytes converts a redis Eval result to []byte.
// Integer → decimal string bytes.
// String/[]byte → as-is bytes.
// Slice → JSON-encoded bytes.
func redisResultToBytes(v any) ([]byte, error) {
	switch val := v.(type) {
	case int64:
		return []byte(strconv.FormatInt(val, 10)), nil
	case string:
		return []byte(val), nil
	case []byte:
		return val, nil
	case nil:
		return nil, nil
	default:
		return json.Marshal(val)
	}
}
