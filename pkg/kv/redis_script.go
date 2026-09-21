package kv

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
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
	r.inflight.Add(1)
	defer r.inflight.Done()

	results := make([]ScriptResult, len(calls))
	if len(calls) == 0 {
		return results
	}

	// Best-effort preload; an empty sha falls back to plain EVAL in-pipeline.
	shas := make([]string, len(calls))
	for i, c := range calls {
		if sha, err := r.loadSHA(ctx, c.Name, c.Script); err == nil {
			shas[i] = sha
		}
	}

	cmds := make([]*redis.Cmd, len(calls))
	// Pipelined returns the first command error; individual results are read
	// from each Cmder below regardless, so the aggregate error is ignored.
	_, _ = r.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, c := range calls {
			if shas[i] != "" {
				cmds[i] = p.EvalSha(ctx, shas[i], c.Keys, c.Args...)
			} else {
				cmds[i] = p.Eval(ctx, c.Script, c.Keys, c.Args...)
			}
		}
		return nil
	})

	for i, cmd := range cmds {
		v, err := cmd.Result()
		if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
			// Script evicted between preload and exec; re-run this one directly
			// (runLua reloads the SHA and falls back to EVAL).
			r.shas.Delete(calls[i].Name)
			b, rerr := r.runLua(ctx, calls[i].Name, calls[i].Script, calls[i].Keys, calls[i].Args...)
			results[i] = ScriptResult{Value: b, Err: rerr}
			continue
		}
		if err != nil {
			results[i] = ScriptResult{Err: err}
			continue
		}
		b, cerr := redisResultToBytes(v)
		results[i] = ScriptResult{Value: b, Err: cerr}
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
