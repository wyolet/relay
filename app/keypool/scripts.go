package keypool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// The breaker scripts read, transition and write a key's record in one round trip, so pods failing or probing the same key concurrently cannot lose each other's updates. Each mirrors a function in transitions.go and returns {prior, changed, record}: the state before the call, whether it wrote, and the record now stored (absent when there is none).
//
// KEYS[1] = circuit key for every script.

// recordLua decodes, encodes and reports a record in its stored form (recordWire).
const recordLua = `
local function num(v)
  v = tonumber(v)
  if v == nil then return 0 end
  return v
end
local function read_record(key)
  local raw = redis.call('GET', key)
  if not raw then return nil end
  local ok, r = pcall(cjson.decode, raw)
  if not ok or type(r) ~= 'table' then return nil end
  local reason = r.reason
  if type(reason) ~= 'string' then reason = '' end
  return {state = num(r.state), open_until_ms = num(r.open_until_ms), backoff_step = num(r.backoff_step),
    last_transition_ms = num(r.last_transition_ms), reason = reason, failures = num(r.failures),
    window_start_ms = num(r.window_start_ms), probe_until_ms = num(r.probe_until_ms)}
end
local function wire(rec)
  local out = {state = rec.state, backoff_step = rec.backoff_step}
  for _, f in ipairs({'open_until_ms', 'last_transition_ms', 'failures', 'window_start_ms', 'probe_until_ms'}) do
    if rec[f] ~= 0 then out[f] = rec[f] end
  end
  if rec.reason ~= '' then out.reason = rec.reason end
  return out
end
local function store(key, rec, ttl)
  local raw = cjson.encode(wire(rec))
  if ttl > 0 then
    redis.call('SET', key, raw, 'PX', ttl)
  else
    redis.call('SET', key, raw)
  end
end
local function result(prior, changed, rec)
  if rec == nil then return cjson.encode({prior = prior, changed = changed}) end
  return cjson.encode({prior = prior, changed = changed, record = wire(rec)})
end
`

// recordFailureScript mirrors applyFailure.
//
// ARGV[1] = now_ms, ARGV[2] = reason, ARGV[3] = threshold (0 = not counted), ARGV[4] = window_ms,
// ARGV[5] = open_ms (used when there is no ladder), ARGV[6] = ttl_ms,
// ARGV[7..] = open time per backoff step in ms (the ladder; may be empty)
const recordFailureScript = recordLua + `
local now = tonumber(ARGV[1])
local reason = ARGV[2]
local threshold = tonumber(ARGV[3])
local window = tonumber(ARGV[4])
local open_ms = tonumber(ARGV[5])
local ttl = tonumber(ARGV[6])
local steps = #ARGV - 6
local rec = read_record(KEYS[1]) or {state = 0, open_until_ms = 0, backoff_step = 0, last_transition_ms = 0,
  reason = '', failures = 0, window_start_ms = 0, probe_until_ms = 0}
local prior = rec.state
local function open(until_ms)
  rec.state = 1
  rec.open_until_ms = until_ms
  rec.last_transition_ms = now
  rec.reason = reason
  rec.failures = 0
  rec.window_start_ms = 0
  rec.probe_until_ms = 0
end
if steps == 0 then
  open(now + open_ms)
elseif rec.state == 1 and (threshold > 0 or rec.reason == reason) then
  return result(prior, false, rec)
else
  local trip = true
  if rec.state == 0 and threshold > 0 then
    if rec.window_start_ms == 0 or now - rec.window_start_ms >= window then
      rec.window_start_ms = now
      rec.failures = 0
    end
    rec.failures = rec.failures + 1
    trip = rec.failures >= threshold
  end
  if trip then
    rec.backoff_step = math.min(rec.backoff_step + 1, steps)
    open(now + tonumber(ARGV[6 + rec.backoff_step]))
  end
end
store(KEYS[1], rec, ttl)
return result(prior, true, rec)
`

// recordSuccessScript mirrors applySuccess.
//
// ARGV[1] = now_ms, ARGV[2] = ttl_ms
const recordSuccessScript = recordLua + `
local now = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local rec = read_record(KEYS[1])
if not rec then return result(0, false, nil) end
local prior = rec.state
if prior == 1 then return result(prior, false, rec) end
if prior ~= 0 then rec.last_transition_ms = now end
rec.state = 0
rec.backoff_step = math.max(rec.backoff_step - 1, 0)
rec.reason = ''
rec.failures = 0
rec.window_start_ms = 0
rec.probe_until_ms = 0
store(KEYS[1], rec, ttl)
return result(prior, true, rec)
`

// claimProbeScript mirrors claimProbe.
//
// ARGV[1] = now_ms, ARGV[2] = lease_ms, ARGV[3] = ttl_ms
const claimProbeScript = recordLua + `
local now = tonumber(ARGV[1])
local lease = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local rec = read_record(KEYS[1])
if not rec then return result(0, false, nil) end
local prior = rec.state
if prior == 1 then
  if now < rec.open_until_ms then return result(prior, false, rec) end
  rec.last_transition_ms = now
elseif prior == 2 then
  if now < rec.probe_until_ms then return result(prior, false, rec) end
else
  return result(prior, false, rec)
end
rec.state = 2
rec.probe_until_ms = now + lease
store(KEYS[1], rec, ttl)
return result(prior, true, rec)
`

const (
	scriptRecordFailure = "keypool.record_failure"
	scriptRecordSuccess = "keypool.record_success"
	scriptClaimProbe    = "keypool.claim_probe"
)

// RegisterScripts installs the Go emulators for keypool's Lua scripts on a
// MemStore, mirroring the Redis-side behaviour exactly. New registers them
// automatically when the store IS a *kv.Mem; test doubles that merely embed
// one must call this on the inner Mem themselves (same convention as
// pkgratelimit.RegisterScripts).
func RegisterScripts(m *kv.Mem) {
	m.RegisterScript(scriptRecordFailure, memRecordScript(func(args []any) (recordTransition, time.Duration, error) {
		op, ttl, err := parseFailureArgs(args)
		return func(rec CircuitRecord, _ bool) (CircuitRecord, bool) { return applyFailure(rec, op) }, ttl, err
	}))
	m.RegisterScript(scriptRecordSuccess, memRecordScript(func(args []any) (recordTransition, time.Duration, error) {
		ints, err := intArgs(args, 2)
		if err != nil {
			return nil, 0, err
		}
		now := fromMillis(ints[0])
		return func(rec CircuitRecord, found bool) (CircuitRecord, bool) {
			if !found {
				return rec, false
			}
			return applySuccess(rec, now)
		}, time.Duration(ints[1]) * time.Millisecond, nil
	}))
	m.RegisterScript(scriptClaimProbe, memRecordScript(func(args []any) (recordTransition, time.Duration, error) {
		ints, err := intArgs(args, 3)
		if err != nil {
			return nil, 0, err
		}
		now, lease := fromMillis(ints[0]), time.Duration(ints[1])*time.Millisecond
		return func(rec CircuitRecord, found bool) (CircuitRecord, bool) {
			if !found {
				return rec, false
			}
			return claimProbe(rec, now, lease)
		}, time.Duration(ints[2]) * time.Millisecond, nil
	}))
}

// recordTransition computes a record's next value from the stored one; found is false when the store held none.
type recordTransition func(rec CircuitRecord, found bool) (CircuitRecord, bool)

func memRecordScript(parse func(args []any) (recordTransition, time.Duration, error)) func(context.Context, *kv.Mem, []string, []any) ([]byte, error) {
	return func(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
		if len(keys) != 1 {
			return nil, fmt.Errorf("keypool: breaker script: expected 1 key, got %d", len(keys))
		}
		apply, ttl, err := parse(args)
		if err != nil {
			return nil, fmt.Errorf("keypool: breaker script: %w", err)
		}
		var out []byte
		lockErr := store.WithLock(ctx, keys, func(ctx context.Context) error {
			out, err = applyRecord(ctx, store, keys[0], apply, ttl)
			return err
		})
		return out, lockErr
	}
}

// applyRecord runs apply on the record under key and writes it back when it changed, returning the scripts' outcome. Atomic only when the caller holds the key's lock.
func applyRecord(ctx context.Context, st kv.Store, key string, apply recordTransition, ttl time.Duration) ([]byte, error) {
	rec, found := recordFrom(st.Get(ctx, key))
	next, changed := apply(rec, found)
	if changed {
		b, err := encodeRecord(next)
		if err != nil {
			return nil, err
		}
		if err := st.Set(ctx, key, b, ttl); err != nil {
			return nil, err
		}
	}
	return encodeOutcome(rec.State, changed, next, found || changed)
}

// recordOutcome is a breaker script's result.
type recordOutcome struct {
	prior   CircuitState
	changed bool
	rec     CircuitRecord
}

type outcomeWire struct {
	Prior   CircuitState    `json:"prior"`
	Changed bool            `json:"changed"`
	Record  json.RawMessage `json:"record,omitempty"`
}

func encodeOutcome(prior CircuitState, changed bool, rec CircuitRecord, hasRecord bool) ([]byte, error) {
	w := outcomeWire{Prior: prior, Changed: changed}
	if hasRecord {
		b, err := encodeRecord(rec)
		if err != nil {
			return nil, err
		}
		w.Record = b
	}
	return json.Marshal(w)
}

func decodeOutcome(raw []byte) (recordOutcome, error) {
	var w outcomeWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return recordOutcome{}, fmt.Errorf("keypool: decode breaker outcome: %w", err)
	}
	out := recordOutcome{prior: w.Prior, changed: w.Changed, rec: CircuitRecord{State: CircuitClosed}}
	if len(w.Record) > 0 {
		rec, err := decodeRecord(w.Record)
		if err != nil {
			return recordOutcome{}, fmt.Errorf("keypool: decode breaker outcome: %w", err)
		}
		out.rec = rec
	}
	return out, nil
}

func failureCall(keyHash string, op failureOp) kv.ScriptCall {
	args := make([]any, 0, 6+len(op.ladder))
	args = append(args, op.now.UnixMilli(), string(op.reason), int64(op.threshold), op.window.Milliseconds(), op.openFor.Milliseconds(), ttlFlat.Milliseconds())
	for _, d := range op.ladder {
		args = append(args, d.Milliseconds())
	}
	return kv.ScriptCall{Name: scriptRecordFailure, Script: recordFailureScript, Keys: []string{circuitKey(keyHash)}, Args: args}
}

func parseFailureArgs(args []any) (failureOp, time.Duration, error) {
	if len(args) < 6 {
		return failureOp{}, 0, fmt.Errorf("record_failure: expected at least 6 args, got %d", len(args))
	}
	reason, ok := args[1].(string)
	if !ok {
		return failureOp{}, 0, fmt.Errorf("record_failure: arg[1] must be a string")
	}
	ints := make([]int64, 0, len(args)-1)
	for i, a := range args {
		if i == 1 {
			continue
		}
		v, err := toInt64(a)
		if err != nil {
			return failureOp{}, 0, fmt.Errorf("record_failure: arg[%d]: %w", i, err)
		}
		ints = append(ints, v)
	}
	op := failureOp{
		now:       fromMillis(ints[0]),
		reason:    CooldownReason(reason),
		threshold: int(ints[1]),
		window:    time.Duration(ints[2]) * time.Millisecond,
		openFor:   time.Duration(ints[3]) * time.Millisecond,
	}
	for _, ms := range ints[5:] {
		op.ladder = append(op.ladder, time.Duration(ms)*time.Millisecond)
	}
	return op, time.Duration(ints[4]) * time.Millisecond, nil
}

func successCall(keyHash string, now time.Time) kv.ScriptCall {
	return kv.ScriptCall{
		Name:   scriptRecordSuccess,
		Script: recordSuccessScript,
		Keys:   []string{circuitKey(keyHash)},
		Args:   []any{now.UnixMilli(), ttlFlat.Milliseconds()},
	}
}

func claimCall(keyHash string, now time.Time) kv.ScriptCall {
	return kv.ScriptCall{
		Name:   scriptClaimProbe,
		Script: claimProbeScript,
		Keys:   []string{circuitKey(keyHash)},
		Args:   []any{now.UnixMilli(), probeLease.Milliseconds(), ttlFlat.Milliseconds()},
	}
}

func intArgs(args []any, n int) ([]int64, error) {
	if len(args) < n {
		return nil, fmt.Errorf("expected %d args, got %d", n, len(args))
	}
	out := make([]int64, n)
	for i := range out {
		v, err := toInt64(args[i])
		if err != nil {
			return nil, fmt.Errorf("arg[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}
