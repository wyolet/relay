package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// PeekScope is one scope's rules for Peek.
type PeekScope struct {
	Scope string
	Rules []Rule
}

// Peek returns, per scope and rule, the count Reserve compares with the rule's amount before a new request's own: requests or tokens in the window, concurrency slots held, a token bucket's drawn tokens, a leaky bucket's level, a session window's count, a spend rule's USD nanos (alert rules included). A fractional count rounds up; a revocation rule reads 0.
//
// It reads the keys Reserve would at the same instant, built by the same code, in one batched round trip for every scope, and writes nothing: no count, no TTL.
func (l *Limiter) Peek(ctx context.Context, scopes []PeekScope) ([][]int64, error) {
	if l.runner == nil {
		return nil, kv.ErrNoScripter
	}
	now := l.clock()
	out := make([][]int64, len(scopes))
	calls := make([]kv.ScriptCall, 0, len(scopes))
	args := make([][]ruleArg, 0, len(scopes))
	at := make([]int, 0, len(scopes))
	for i, s := range scopes {
		out[i] = make([]int64, len(s.Rules))
		if len(s.Rules) == 0 {
			continue
		}
		call, ra, err := buildPeekCall(s.Scope, s.Rules, now)
		if err != nil {
			return nil, err
		}
		calls, args, at = append(calls, call), append(args, ra), append(at, i)
	}
	if len(calls) == 0 {
		return out, nil
	}
	var results []kv.ScriptResult
	if batcher, ok := l.runner.(kv.BatchScripter); ok && len(calls) > 1 {
		results = batcher.RunScriptBatch(ctx, calls)
	} else {
		results = make([]kv.ScriptResult, len(calls))
		for i, c := range calls {
			results[i].Value, results[i].Err = l.runner.RunScript(ctx, c.Name, c.Script, c.Keys, c.Args...)
		}
	}
	for j, r := range results {
		if r.Err != nil {
			return nil, fmt.Errorf("limit: peek script: %w", r.Err)
		}
		var raw []string
		if err := json.Unmarshal(r.Value, &raw); err != nil {
			return nil, fmt.Errorf("limit: decode peek result: %w", err)
		}
		if len(raw) != len(args[j]) {
			return nil, fmt.Errorf("limit: peek returned %d readings for %d rules", len(raw), len(args[j]))
		}
		for k, ra := range args[j] {
			used, err := peekCount(ra, raw[k], now.UnixMilli())
			if err != nil {
				return nil, err
			}
			out[at[j]][k] = used
		}
	}
	return out, nil
}

// buildPeekCall renders the read of rules with the reserve script's own key builder. Alert spend rules are read like hard ones: Reserve skips them only because it does not check them.
func buildPeekCall(scope string, rules []Rule, now time.Time) (kv.ScriptCall, []ruleArg, error) {
	read, copied := rules, false
	for i, r := range rules {
		if r.Meter == MeterSpend && r.Alert {
			if !copied {
				read, copied = append([]Rule(nil), rules...), true
			}
			read[i].Alert = false
		}
	}
	keys, ruleArgs, err := buildReserveArgs(scope, read, now)
	if err != nil {
		return kv.ScriptCall{}, nil, err
	}
	rulesJSON, err := json.Marshal(&ruleArgs)
	if err != nil {
		return kv.ScriptCall{}, nil, fmt.Errorf("limit: marshal rules: %w", err)
	}
	return kv.ScriptCall{Name: "limit.peek", Script: peekLuaScript, Keys: keys, Args: []any{now.UnixMilli(), rulesJSON}}, ruleArgs, nil
}

// peekLuaScript reads, per rule of rules_json (the reserve script's rule args), the raw state Reserve would read, as a string: one value, or two joined by a comma; an empty field is a missing key or hash field. It calls no write command.
//
//	ARGV[1] = now_ms, ARGV[2] = rules_json
const peekLuaScript = `
local rules = cjson.decode(ARGV[2])
local out = {}
local function get(i)
  local v = redis.call('GET', KEYS[i])
  if v then return v end
  return ''
end
local function fields(i, a, b)
  local v = redis.call('HMGET', KEYS[i], a, b)
  return (v[1] or '') .. ',' .. (v[2] or '')
end
for i, r in ipairs(rules) do
  local s = ''
  if r.meter == 'revoked' then
    s = ''
  elseif r.meter == 'concurrency' then
    s = tostring(redis.call('ZCOUNT', KEYS[r.con_key_idx], '(' .. ARGV[1], '+inf'))
  elseif r.meter == 'spend' then
    s = get(r.spend_key_idx)
  elseif r.fw_key_idx then
    s = get(r.fw_key_idx)
  elseif r.tb_key_idx then
    s = fields(r.tb_key_idx, 'tokens', 'last_ms')
  elseif r.lb_key_idx then
    s = fields(r.lb_key_idx, 'level', 'last_ms')
  elseif r.sw_key_idx then
    s = fields(r.sw_key_idx, 'count', 'anchor_ms')
  elseif r.cur_key_idx then
    s = get(r.cur_key_idx) .. ',' .. get(r.prev_key_idx)
  end
  out[i] = s
end
return out
`

// memPeekImpl is the Go emulator for peekLuaScript.
func memPeekImpl(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("limit.peek: expected 2 args, got %d", len(args))
	}
	nowMs, err := toInt64(args[0])
	if err != nil {
		return nil, fmt.Errorf("limit.peek: arg[0] now_ms: %w", err)
	}
	rulesJSON, ok := toBytes(args[1])
	if !ok {
		return nil, fmt.Errorf("limit.peek: arg[1] must be string")
	}
	var rules []ruleArg
	if err := json.Unmarshal(rulesJSON, &rules); err != nil {
		return nil, fmt.Errorf("limit.peek: parse rules: %w", err)
	}
	get := func(i int) string {
		b, err := store.Get(ctx, keys[i-1])
		if err != nil {
			return ""
		}
		return string(b)
	}
	field := func(i int, f string) string {
		b, err := store.HGet(ctx, keys[i-1], f)
		if err != nil {
			return ""
		}
		return string(b)
	}
	out := make([]string, len(rules))
	for i, r := range rules {
		switch {
		case r.Meter == MeterRevoked:
		case r.Meter == "concurrency":
			leases, err := memReadLeases(ctx, store, keys[r.ConKeyIdx-1])
			if err != nil {
				return nil, err
			}
			held, _ := leases.live(nowMs)
			out[i] = strconv.FormatInt(held, 10)
		case r.Meter == MeterSpend:
			out[i] = get(r.SpendKeyIdx)
		case r.FwKeyIdx > 0:
			out[i] = get(r.FwKeyIdx)
		case r.TbKeyIdx > 0:
			out[i] = field(r.TbKeyIdx, "tokens") + "," + field(r.TbKeyIdx, "last_ms")
		case r.LbKeyIdx > 0:
			out[i] = field(r.LbKeyIdx, "level") + "," + field(r.LbKeyIdx, "last_ms")
		case r.SwKeyIdx > 0:
			out[i] = field(r.SwKeyIdx, "count") + "," + field(r.SwKeyIdx, "anchor_ms")
		case r.CurKeyIdx > 0:
			out[i] = get(r.CurKeyIdx) + "," + get(r.PrevKeyIdx)
		}
	}
	return json.Marshal(out)
}

// peekCount turns one rule's raw state into its count at nowMs, with the reserve script's refill, drain and window arithmetic.
func peekCount(r ruleArg, raw string, nowMs int64) (int64, error) {
	a, b, _ := strings.Cut(raw, ",")
	switch {
	case r.Meter == MeterRevoked:
		return 0, nil
	case r.Meter == "concurrency", r.Meter == MeterSpend, r.FwKeyIdx > 0:
		// Whole counts, parsed as integers: a spend in nanos outgrows a float's exact range.
		if a == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("limit: peek %s: %w", r.RuleKey, err)
		}
		return max(n, 0), nil
	}
	first, err := peekNumber(a)
	if err != nil {
		return 0, fmt.Errorf("limit: peek %s: %w", r.RuleKey, err)
	}
	second, err := peekNumber(b)
	if err != nil {
		return 0, fmt.Errorf("limit: peek %s: %w", r.RuleKey, err)
	}
	amount, winMs := float64(r.Amount), float64(r.WindowMs)
	var used float64
	switch {
	case r.TbKeyIdx > 0:
		if !first.set {
			break
		}
		tokens := first.v
		if elapsed := float64(nowMs) - second.or(float64(nowMs)); elapsed > 0 && winMs > 0 {
			tokens = math.Min(tokens+elapsed*amount*1000/winMs, amount*1000)
		}
		used = amount - tokens/1000
	case r.LbKeyIdx > 0:
		level := first.or(0)
		if elapsed := float64(nowMs) - second.or(float64(nowMs)); elapsed > 0 && winMs > 0 {
			level = math.Max(level-elapsed*amount*1000/winMs, 0)
		}
		used = level / 1000
	case r.SwKeyIdx > 0:
		if first.set && second.set && float64(nowMs) < second.v+winMs {
			used = first.v
		}
	case r.CurKeyIdx > 0 && winMs > 0:
		bucketStart := math.Floor(float64(nowMs)/winMs) * winMs
		used = first.or(0) + second.or(0)*(1-(float64(nowMs)-bucketStart)/winMs)
	}
	// The slack keeps float noise on a whole count from rounding it up by one.
	return int64(math.Ceil(math.Max(used, 0) - 1e-9)), nil
}

type peekValue struct {
	v   float64
	set bool
}

func (p peekValue) or(def float64) float64 {
	if p.set {
		return p.v
	}
	return def
}

// peekNumber parses a raw field; the Lua scripts may store a float where the emulator stores an integer.
func peekNumber(s string) (peekValue, error) {
	if s == "" {
		return peekValue{}, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return peekValue{}, err
	}
	return peekValue{v: v, set: true}, nil
}
