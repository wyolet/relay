package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/kv"
)

// reserveLuaScript is the atomic Reserve script run on Redis via EVALSHA.
//
// Wire format:
//
//	KEYS  = all counter/state keys, positionally matched to rules_json entries.
//	ARGV[1] = now_ms  (int)
//	ARGV[2] = rules_json (JSON array of ruleArg)
//	ARGV[3] = token (unique reservation ID, stored in the result)
//
// Returns JSON-encoded reserveResult.
//
// Strategies per-rule (r.strategy):
//
//	"sliding-window"  — two-bucket weighted interpolation (legacy default)
//	"fixed-window"    — single counter per floor(now/window) bucket
//	"token-bucket"    — lazy-refill hash {tokens, last_ms}; refund on cancel
//	"leaky-bucket"    — lazy-drain hash {level, last_ms}; refund on cancel
//	"session-window"  — anchored hash {count, anchor_ms}; refund on cancel
//	"" (concurrency)  — gauge counter, ignores strategy
const reserveLuaScript = `
-- Reserve: per-rule strategy dispatch.
-- ARGV[1]=now_ms, ARGV[2]=rules_json, ARGV[3]=token
local now_ms   = tonumber(ARGV[1])
local rules    = cjson.decode(ARGV[2])
local tok      = ARGV[3]

local inc_con  = {}  -- {key, ttl_ms} — concurrency keys incremented
local inc_req  = {}  -- {key, ttl_ms} — request/fixed-window keys incremented

local function rollback()
  for _, v in ipairs(inc_req) do redis.call('INCRBY', v[1], -1) end
  for _, v in ipairs(inc_con) do redis.call('INCRBY', v[1], -1) end
end

for i, r in ipairs(rules) do
  local meter    = r.meter
  local amount   = tonumber(r.amount)
  local win_ms   = tonumber(r.window_ms)
  local strategy = r.strategy or "sliding-window"

  -- ── concurrency: gauge counter, strategy ignored ───────────────────────────
  if meter == "concurrency" then
    local con_key = KEYS[r.con_key_idx]
    local ttl_ms  = win_ms * 5

    local new_val = redis.call('INCRBY', con_key, 1)
    redis.call('PEXPIRE', con_key, ttl_ms)

    if new_val > amount then
      redis.call('INCRBY', con_key, -1)
      rollback()
      return cjson.encode({exceeded=true, retry_after_ms=win_ms,
        parent_kind=r.parent_kind, parent_name=r.parent_name,
        rule_name=r.rule_name, meter=meter})
    end
    table.insert(inc_con, {con_key, ttl_ms})

  -- ── tokens / tokens.X: peek only at Reserve; Commit increments ─────────────
  elseif meter == "tokens" or meter:sub(1,7) == "tokens." then
    if strategy == "sliding-window" then
      local cur_key  = KEYS[r.cur_key_idx]
      local prev_key = KEYS[r.prev_key_idx]
      local cur_val  = tonumber(redis.call('GET', cur_key) or "0") or 0
      local prev_val = tonumber(redis.call('GET', prev_key) or "0") or 0
      local bucket_start_ms = math.floor(now_ms / win_ms) * win_ms
      local frac = (now_ms - bucket_start_ms) / win_ms
      local rate  = cur_val + prev_val * (1.0 - frac)
      if rate >= amount then
        rollback()
        local retry_ms = win_ms - (now_ms - bucket_start_ms)
        return cjson.encode({exceeded=true, retry_after_ms=math.floor(retry_ms),
          parent_kind=r.parent_kind, parent_name=r.parent_name,
          rule_name=r.rule_name, meter=meter})
      end
      -- no inc_req entry — tokens incremented at Commit
    else
      -- token-bucket/leaky-bucket: post-hoc too; same peek: always passes at
      -- Reserve time (actual deduct happens at Commit). No pre-check for now.
      -- This matches the sliding-window tokens pattern.
    end

  -- ── requests meter: strategy-specific ─────────────────────────────────────
  elseif strategy == "sliding-window" then
    local cur_key  = KEYS[r.cur_key_idx]
    local prev_key = KEYS[r.prev_key_idx]
    local ttl_ms   = win_ms * 2

    local new_cur = redis.call('INCRBY', cur_key, 1)
    redis.call('PEXPIRE', cur_key, ttl_ms)

    local prev_val = tonumber(redis.call('GET', prev_key) or "0") or 0
    local bucket_start_ms = math.floor(now_ms / win_ms) * win_ms
    local elapsed_ms = now_ms - bucket_start_ms
    local frac = elapsed_ms / win_ms
    local rate = new_cur + prev_val * (1.0 - frac)

    if rate > amount then
      redis.call('INCRBY', cur_key, -1)
      rollback()
      local retry_ms = win_ms - elapsed_ms
      if prev_val > 0 then
        local need = (amount - (new_cur - 1)) / prev_val
        local frac_t = 1.0 - need
        if frac_t > 0 and frac_t < 1.0 then
          local wait = frac_t * win_ms - elapsed_ms
          if wait > 0 and wait < retry_ms then retry_ms = wait end
        end
      end
      return cjson.encode({exceeded=true, retry_after_ms=math.floor(retry_ms),
        parent_kind=r.parent_kind, parent_name=r.parent_name,
        rule_name=r.rule_name, meter=meter})
    end
    table.insert(inc_req, {cur_key, ttl_ms})

  elseif strategy == "fixed-window" then
    local fw_key      = KEYS[r.fw_key_idx]
    local ttl_ms      = win_ms * 2
    local bucket_start_ms = math.floor(now_ms / win_ms) * win_ms

    local new_val = redis.call('INCRBY', fw_key, 1)
    redis.call('PEXPIRE', fw_key, ttl_ms)

    if new_val > amount then
      redis.call('INCRBY', fw_key, -1)
      rollback()
      local retry_ms = win_ms - (now_ms - bucket_start_ms)
      return cjson.encode({exceeded=true, retry_after_ms=math.floor(retry_ms),
        parent_kind=r.parent_kind, parent_name=r.parent_name,
        rule_name=r.rule_name, meter=meter})
    end
    table.insert(inc_req, {fw_key, ttl_ms})

  elseif strategy == "token-bucket" then
    -- State hash: {tokens (scaled *1000 as int), last_ms}
    -- refill_rate = amount / win_ms  tokens/ms
    -- TTL = win_ms * 2
    local state_key = KEYS[r.tb_key_idx]
    local ttl_ms    = win_ms * 2

    local raw = redis.call('HMGET', state_key, 'tokens', 'last_ms')
    local tokens_i = tonumber(raw[1])
    local last_ms  = tonumber(raw[2])

    local tokens
    if tokens_i == nil then
      tokens  = amount * 1000
      last_ms = now_ms
    else
      tokens  = tokens_i
    end

    -- lazy refill
    local elapsed = now_ms - last_ms
    if elapsed > 0 then
      local refill = elapsed * amount * 1000 / win_ms
      tokens = tokens + refill
      if tokens > amount * 1000 then tokens = amount * 1000 end
    end

    -- cost = 1 request (1000 scaled)
    local cost = 1000
    if tokens < cost then
      local retry_ms = math.ceil((cost - tokens) * win_ms / (amount * 1000))
      rollback()
      return cjson.encode({exceeded=true, retry_after_ms=retry_ms,
        parent_kind=r.parent_kind, parent_name=r.parent_name,
        rule_name=r.rule_name, meter=meter})
    end

    tokens = tokens - cost
    redis.call('HMSET', state_key, 'tokens', tokens, 'last_ms', now_ms)
    redis.call('PEXPIRE', state_key, ttl_ms)

  elseif strategy == "leaky-bucket" then
    -- State hash: {level (scaled *1000), last_ms}
    -- leak_rate = amount / win_ms  per ms
    local state_key = KEYS[r.lb_key_idx]
    local ttl_ms    = win_ms * 2

    local raw = redis.call('HMGET', state_key, 'level', 'last_ms')
    local level_i = tonumber(raw[1])
    local last_ms = tonumber(raw[2])

    local level
    if level_i == nil then
      level   = 0
      last_ms = now_ms
    else
      level = level_i
    end

    -- lazy drain
    local elapsed = now_ms - last_ms
    if elapsed > 0 then
      local drained = elapsed * amount * 1000 / win_ms
      level = level - drained
      if level < 0 then level = 0 end
    end

    local cost = 1000
    if level + cost > amount * 1000 then
      local retry_ms = math.ceil((level + cost - amount * 1000) * win_ms / (amount * 1000))
      rollback()
      return cjson.encode({exceeded=true, retry_after_ms=retry_ms,
        parent_kind=r.parent_kind, parent_name=r.parent_name,
        rule_name=r.rule_name, meter=meter})
    end

    level = level + cost
    redis.call('HMSET', state_key, 'level', level, 'last_ms', now_ms)
    redis.call('PEXPIRE', state_key, ttl_ms)

  elseif strategy == "session-window" then
    -- State hash: {count, anchor_ms}. Window arms on first request after a
    -- reset and runs for win_ms; counter is a hard integer (no refill).
    local state_key = KEYS[r.sw_key_idx]
    local ttl_ms    = win_ms * 2

    local raw = redis.call('HMGET', state_key, 'count', 'anchor_ms')
    local count_v  = tonumber(raw[1])
    local anchor_v = tonumber(raw[2])

    local count, anchor_ms
    if count_v == nil or anchor_v == nil or now_ms >= anchor_v + win_ms then
      count = 0
      anchor_ms = now_ms
    else
      count = count_v
      anchor_ms = anchor_v
    end

    count = count + 1
    if count > amount then
      rollback()
      local retry_ms = anchor_ms + win_ms - now_ms
      return cjson.encode({exceeded=true, retry_after_ms=math.floor(retry_ms),
        parent_kind=r.parent_kind, parent_name=r.parent_name,
        rule_name=r.rule_name, meter=meter})
    end

    redis.call('HMSET', state_key, 'count', count, 'anchor_ms', anchor_ms)
    redis.call('PEXPIRE', state_key, ttl_ms)
  end
end

return cjson.encode({exceeded=false, token=tok})
`

// commitLuaScript is the atomic Commit script.
//
// Wire format:
//
//	KEYS[1]             = guard key (idempotency)
//	KEYS[2..1+n_con]    = concurrency keys to decrement
//	KEYS[...]           = token bucket cur keys (post-hoc increment for tokens meter)
//	KEYS[...]           = token-bucket state hash keys (refund on cancel)
//	KEYS[...]           = leaky-bucket state hash keys (refund on cancel)
//	ARGV[1] = reservation token
//	ARGV[2] = guard_ttl_ms
//	ARGV[3] = n_con
//	ARGV[4] = n_tok
//	ARGV[5] = tok_amounts_json
//	ARGV[6] = tok_ttl_ms
//	ARGV[7] = cancelled (0 or 1)
//	ARGV[8]  = tb_refunds_json  — array of {key_idx, cost_scaled, burst} for token-bucket refunds
//	ARGV[9]  = lb_refunds_json  — array of {key_idx, cost_scaled} for leaky-bucket refunds
//	ARGV[10] = sw_refunds_json  — array of {key_idx, count} for session-window refunds
//
// Returns "ok" or "noop" (duplicate).
const commitLuaScript = `
-- Commit: decrement concurrency, post-hoc token increment, refund on cancel.
local guard_key   = KEYS[1]
local tok         = ARGV[1]
local guard_ttl   = tonumber(ARGV[2])
local n_con       = tonumber(ARGV[3])
local n_tok       = tonumber(ARGV[4])
local tok_amounts = cjson.decode(ARGV[5])
local tok_ttl_ms  = tonumber(ARGV[6])
local cancelled   = tonumber(ARGV[7]) == 1
local tb_refunds  = cjson.decode(ARGV[8])
local lb_refunds  = cjson.decode(ARGV[9])
local sw_refunds  = cjson.decode(ARGV[10])

if redis.call('EXISTS', guard_key) == 1 then
  return "noop"
end

-- decrement concurrency counters (always, even on cancel)
for i = 2, 1 + n_con do
  redis.call('INCRBY', KEYS[i], -1)
end

-- post-hoc token increment (only when not cancelled)
if not cancelled then
  for j = 1, n_tok do
    local amount = tonumber(tok_amounts[j]) or 0
    if amount > 0 then
      local key_idx = 1 + n_con + j
      redis.call('INCRBY', KEYS[key_idx], amount)
      redis.call('PEXPIRE', KEYS[key_idx], tok_ttl_ms)
    end
  end
end

-- token-bucket refund on cancel: add back deducted tokens
if cancelled then
  for _, entry in ipairs(tb_refunds) do
    local key_idx   = tonumber(entry[1])
    local cost      = tonumber(entry[2])
    local cur_i     = tonumber(redis.call('HGET', KEYS[key_idx], 'tokens'))
    if cur_i ~= nil then
      local burst = tonumber(entry[3])
      local new_t = cur_i + cost
      if new_t > burst * 1000 then new_t = burst * 1000 end
      redis.call('HSET', KEYS[key_idx], 'tokens', new_t)
    end
  end

  for _, entry in ipairs(lb_refunds) do
    local key_idx = tonumber(entry[1])
    local cost    = tonumber(entry[2])
    local cur_i   = tonumber(redis.call('HGET', KEYS[key_idx], 'level'))
    if cur_i ~= nil then
      local new_l = cur_i - cost
      if new_l < 0 then new_l = 0 end
      redis.call('HSET', KEYS[key_idx], 'level', new_l)
    end
  end

  for _, entry in ipairs(sw_refunds) do
    local key_idx = tonumber(entry[1])
    local cost    = tonumber(entry[2])
    local cur_i   = tonumber(redis.call('HGET', KEYS[key_idx], 'count'))
    if cur_i ~= nil then
      local new_c = cur_i - cost
      if new_c < 0 then new_c = 0 end
      redis.call('HSET', KEYS[key_idx], 'count', new_c)
    end
  end
end

redis.call('SET', guard_key, tok, 'PX', guard_ttl)
return "ok"
`

// ruleArg is the JSON shape passed to the Reserve Lua script per rule.
type ruleArg struct {
	Meter      string `json:"meter"`
	Amount     int64  `json:"amount"`
	WindowMs   int64  `json:"window_ms"`
	Strategy   string `json:"strategy"`
	ParentKind string `json:"parent_kind"`
	ParentName string `json:"parent_name"`
	RuleName   string `json:"rule_name"`
	// key indices (1-based into KEYS slice) — set based on strategy
	CurKeyIdx int `json:"cur_key_idx,omitempty"`
	PrevKeyIdx int `json:"prev_key_idx,omitempty"`
	ConKeyIdx  int `json:"con_key_idx,omitempty"`
	FwKeyIdx   int `json:"fw_key_idx,omitempty"`
	TbKeyIdx   int `json:"tb_key_idx,omitempty"`
	LbKeyIdx   int `json:"lb_key_idx,omitempty"`
	SwKeyIdx   int `json:"sw_key_idx,omitempty"`
}

// reserveResult is decoded from the Reserve script return value.
type reserveResult struct {
	Exceeded     bool   `json:"exceeded"`
	Token        string `json:"token"`
	RetryAfterMs int64  `json:"retry_after_ms"`
	ParentKind   string `json:"parent_kind"`
	ParentName   string `json:"parent_name"`
	RuleName     string `json:"rule_name"`
	Meter        string `json:"meter"`
}

// buildReserveArgs builds (keys, ruleArgs) for the Reserve script.
func buildReserveArgs(poolName string, rules []catalog.ResolvedRule, now time.Time) (keys []string, ruleArgs []ruleArg, err error) {
	seen := make(map[string]int) // key -> 1-based index

	addKey := func(k string) int {
		if idx, ok := seen[k]; ok {
			return idx
		}
		keys = append(keys, k)
		idx := len(keys) // 1-based
		seen[k] = idx
		return idx
	}

	ruleArgs = make([]ruleArg, 0, len(rules))
	for _, rule := range rules {
		w := rule.Window
		if w == 0 && rule.RateLimit != nil {
			w = rule.RateLimit.Spec.Window
		}
		rlName := rule.RateLimitName
		if rlName == "" && rule.RateLimit != nil {
			rlName = rule.RateLimit.Metadata.Name
		}
		meter := rule.Rule.Meter
		if meter == "" {
			meter = string(rule.Meter)
		}
		strategy := string(rule.Strategy)
		if strategy == "" && rule.RateLimit != nil {
			strategy = string(rule.RateLimit.Spec.Strategy)
		}
		if strategy == "" {
			strategy = string(catalog.StrategyTokenBucket)
		}

		ra := ruleArg{
			Meter:      meter,
			Amount:     rule.Rule.Amount,
			WindowMs:   w.Milliseconds(),
			Strategy:   strategy,
			ParentKind: string(rule.ParentKind),
			ParentName: rule.ParentName,
			RuleName:   rlName,
		}

		m := catalog.Meter(meter)
		switch m {
		case catalog.MeterConcurrency:
			ra.ConKeyIdx = addKey(concurrencyKey(poolName, rule))
		case catalog.MeterTokens:
			// tokens meter is always post-hoc (peek at Reserve); sliding-window keys
			// needed for peek; other strategies just peek without key read.
			cur, prev := windowBuckets(now, w)
			ra.CurKeyIdx = addKey(bucketKey(poolName, rule, cur))
			ra.PrevKeyIdx = addKey(bucketKey(poolName, rule, prev))
		default:
			// requests or tokens.X
			switch catalog.RateLimitStrategy(strategy) {
			case catalog.StrategyFixedWindow:
				bucketStartMs := (now.UnixMilli() / w.Milliseconds()) * w.Milliseconds()
				ra.FwKeyIdx = addKey(fixedWindowKey(poolName, rule, bucketStartMs))
			case catalog.StrategyTokenBucket:
				ra.TbKeyIdx = addKey(tbStateKey(poolName, rule))
			case catalog.StrategyLeakyBucket:
				ra.LbKeyIdx = addKey(lbStateKey(poolName, rule))
			case catalog.StrategySessionWindow:
				ra.SwKeyIdx = addKey(swStateKey(poolName, rule))
			default: // sliding-window
				cur, prev := windowBuckets(now, w)
				ra.CurKeyIdx = addKey(bucketKey(poolName, rule, cur))
				ra.PrevKeyIdx = addKey(bucketKey(poolName, rule, prev))
			}
		}
		ruleArgs = append(ruleArgs, ra)
	}
	return keys, ruleArgs, nil
}

// RegisterScripts registers the Go emulators for limit.reserve and limit.commit
// on the given MemStore. Called once per Limiter constructed from a MemStore.
func RegisterScripts(m *kv.Mem) {
	m.RegisterScript("limit.reserve", memReserveImpl)
	m.RegisterScript("limit.commit", memCommitImpl)
}

// memReserveImpl is the Go emulator for reserveLuaScript.
func memReserveImpl(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
	if len(args) < 3 {
		return nil, fmt.Errorf("limit.reserve: expected 3 args, got %d", len(args))
	}
	nowMs, err := toInt64(args[0])
	if err != nil {
		return nil, fmt.Errorf("limit.reserve: arg[0] now_ms: %w", err)
	}
	rulesJSON, ok := args[1].(string)
	if !ok {
		return nil, fmt.Errorf("limit.reserve: arg[1] must be string")
	}
	token, ok := args[2].(string)
	if !ok {
		return nil, fmt.Errorf("limit.reserve: arg[2] must be string")
	}

	var rules []ruleArg
	if err := json.Unmarshal([]byte(rulesJSON), &rules); err != nil {
		return nil, fmt.Errorf("limit.reserve: parse rules: %w", err)
	}

	type incEntry struct{ key string }
	var incReq []incEntry
	var incCon []incEntry

	var result reserveResult

	lockErr := store.WithLock(ctx, keys, func(ctx context.Context) error {
		rollback := func() {
			for _, v := range incReq {
				_, _ = store.Incr(ctx, v.key, -1)
			}
			for _, v := range incCon {
				_, _ = store.Incr(ctx, v.key, -1)
			}
		}

		for _, r := range rules {
			amount := r.Amount
			winMs := r.WindowMs
			strategy := r.Strategy

			switch r.Meter {
			case "concurrency":
				conKey := keys[r.ConKeyIdx-1]
				ttl := time.Duration(winMs*5) * time.Millisecond

				newVal, err := store.Incr(ctx, conKey, 1)
				if err != nil {
					return err
				}
				_ = store.Expire(ctx, conKey, ttl)

				if newVal > amount {
					_, _ = store.Incr(ctx, conKey, -1)
					rollback()
					result = reserveResult{
						Exceeded:     true,
						RetryAfterMs: winMs,
						ParentKind:   r.ParentKind,
						ParentName:   r.ParentName,
						RuleName:     r.RuleName,
						Meter:        r.Meter,
					}
					return nil
				}
				incCon = append(incCon, incEntry{conKey})

			default:
				// tokens / tokens.X — always post-hoc; for sliding-window peek:
				if r.Meter == "tokens" || len(r.Meter) > 7 && r.Meter[:7] == "tokens." {
					if strategy == string(catalog.StrategySlidingWindow) {
						curKey := keys[r.CurKeyIdx-1]
						prevKey := keys[r.PrevKeyIdx-1]
						curVal, _ := memReadCounter(ctx, store, curKey)
						prevVal, _ := memReadCounter(ctx, store, prevKey)
						bucketStart := (nowMs / winMs) * winMs
						frac := float64(nowMs-bucketStart) / float64(winMs)
						rate := float64(curVal) + float64(prevVal)*(1.0-frac)
						if rate >= float64(amount) {
							rollback()
							retryMs := float64(winMs) - float64(nowMs-bucketStart)
							result = reserveResult{
								Exceeded:     true,
								RetryAfterMs: int64(math.Floor(retryMs)),
								ParentKind:   r.ParentKind,
								ParentName:   r.ParentName,
								RuleName:     r.RuleName,
								Meter:        r.Meter,
							}
							return nil
						}
					}
					// other strategies: peek always passes for tokens meter
					continue
				}

				// requests / non-tokens meters
				switch catalog.RateLimitStrategy(strategy) {
				case catalog.StrategySlidingWindow:
					curKey := keys[r.CurKeyIdx-1]
					prevKey := keys[r.PrevKeyIdx-1]
					ttl := time.Duration(winMs*2) * time.Millisecond

					newCur, err := store.Incr(ctx, curKey, 1)
					if err != nil {
						return err
					}
					_ = store.Expire(ctx, curKey, ttl)

					prevVal, _ := memReadCounter(ctx, store, prevKey)
					bucketStart := (nowMs / winMs) * winMs
					elapsedMs := nowMs - bucketStart
					frac := float64(elapsedMs) / float64(winMs)
					rate := float64(newCur) + float64(prevVal)*(1.0-frac)

					if rate > float64(amount) {
						_, _ = store.Incr(ctx, curKey, -1)
						rollback()
						retryMs := float64(winMs) - float64(elapsedMs)
						if prevVal > 0 {
							need := float64(amount-(newCur-1)) / float64(prevVal)
							fracT := 1.0 - need
							if fracT > 0 && fracT < 1.0 {
								wait := fracT*float64(winMs) - float64(elapsedMs)
								if wait > 0 && wait < retryMs {
									retryMs = wait
								}
							}
						}
						result = reserveResult{
							Exceeded:     true,
							RetryAfterMs: int64(math.Floor(retryMs)),
							ParentKind:   r.ParentKind,
							ParentName:   r.ParentName,
							RuleName:     r.RuleName,
							Meter:        r.Meter,
						}
						return nil
					}
					incReq = append(incReq, incEntry{curKey})

				case catalog.StrategyFixedWindow:
					fwKey := keys[r.FwKeyIdx-1]
					ttl := time.Duration(winMs*2) * time.Millisecond
					bucketStart := (nowMs / winMs) * winMs

					newVal, err := store.Incr(ctx, fwKey, 1)
					if err != nil {
						return err
					}
					_ = store.Expire(ctx, fwKey, ttl)

					if newVal > amount {
						_, _ = store.Incr(ctx, fwKey, -1)
						rollback()
						retryMs := winMs - (nowMs - bucketStart)
						result = reserveResult{
							Exceeded:     true,
							RetryAfterMs: retryMs,
							ParentKind:   r.ParentKind,
							ParentName:   r.ParentName,
							RuleName:     r.RuleName,
							Meter:        r.Meter,
						}
						return nil
					}
					incReq = append(incReq, incEntry{fwKey})

				case catalog.StrategyTokenBucket:
					stateKey := keys[r.TbKeyIdx-1]
					ttl := time.Duration(winMs*2) * time.Millisecond

					tokensI, err := memHGetInt(ctx, store, stateKey, "tokens")
					if err != nil {
						return err
					}
					lastMs, err := memHGetInt(ctx, store, stateKey, "last_ms")
					if err != nil {
						return err
					}

					var tokens int64
					if tokensI < 0 { // key absent
						tokens = amount * 1000
						lastMs = nowMs
					} else {
						tokens = tokensI
					}

					elapsed := nowMs - lastMs
					if elapsed > 0 {
						refill := elapsed * amount * 1000 / winMs
						tokens += refill
						if tokens > amount*1000 {
							tokens = amount * 1000
						}
					}

					const cost = int64(1000)
					if tokens < cost {
						rollback()
						retryMs := int64(math.Ceil(float64(cost-tokens) * float64(winMs) / float64(amount*1000)))
						result = reserveResult{
							Exceeded:     true,
							RetryAfterMs: retryMs,
							ParentKind:   r.ParentKind,
							ParentName:   r.ParentName,
							RuleName:     r.RuleName,
							Meter:        r.Meter,
						}
						return nil
					}
					tokens -= cost
					_ = memHSetInt(ctx, store, stateKey, "tokens", tokens, ttl)
					_ = memHSetInt(ctx, store, stateKey, "last_ms", nowMs, ttl)

				case catalog.StrategyLeakyBucket:
					stateKey := keys[r.LbKeyIdx-1]
					ttl := time.Duration(winMs*2) * time.Millisecond

					levelI, err := memHGetInt(ctx, store, stateKey, "level")
					if err != nil {
						return err
					}
					lastMs, err := memHGetInt(ctx, store, stateKey, "last_ms")
					if err != nil {
						return err
					}

					var level int64
					if levelI < 0 { // key absent
						level = 0
						lastMs = nowMs
					} else {
						level = levelI
					}

					elapsed := nowMs - lastMs
					if elapsed > 0 {
						drained := elapsed * amount * 1000 / winMs
						level -= drained
						if level < 0 {
							level = 0
						}
					}

					const cost = int64(1000)
					if level+cost > amount*1000 {
						rollback()
						retryMs := int64(math.Ceil(float64(level+cost-amount*1000) * float64(winMs) / float64(amount*1000)))
						result = reserveResult{
							Exceeded:     true,
							RetryAfterMs: retryMs,
							ParentKind:   r.ParentKind,
							ParentName:   r.ParentName,
							RuleName:     r.RuleName,
							Meter:        r.Meter,
						}
						return nil
					}
					level += cost
					_ = memHSetInt(ctx, store, stateKey, "level", level, ttl)
					_ = memHSetInt(ctx, store, stateKey, "last_ms", nowMs, ttl)

				case catalog.StrategySessionWindow:
					stateKey := keys[r.SwKeyIdx-1]
					ttl := time.Duration(winMs*2) * time.Millisecond

					countV, err := memHGetInt(ctx, store, stateKey, "count")
					if err != nil {
						return err
					}
					anchorV, err := memHGetInt(ctx, store, stateKey, "anchor_ms")
					if err != nil {
						return err
					}

					var count, anchorMs int64
					if countV < 0 || anchorV < 0 || nowMs >= anchorV+winMs {
						count = 0
						anchorMs = nowMs
					} else {
						count = countV
						anchorMs = anchorV
					}

					count++
					if count > amount {
						rollback()
						retryMs := anchorMs + winMs - nowMs
						result = reserveResult{
							Exceeded:     true,
							RetryAfterMs: retryMs,
							ParentKind:   r.ParentKind,
							ParentName:   r.ParentName,
							RuleName:     r.RuleName,
							Meter:        r.Meter,
						}
						return nil
					}
					_ = memHSetInt(ctx, store, stateKey, "count", count, ttl)
					_ = memHSetInt(ctx, store, stateKey, "anchor_ms", anchorMs, ttl)
				}
			}
		}
		result = reserveResult{Exceeded: false, Token: token}
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return json.Marshal(result)
}

// memReadCounter reads an int64 from MemStore; missing key = 0.
func memReadCounter(ctx context.Context, store *kv.Mem, key string) (int64, error) {
	b, err := store.Get(ctx, key)
	if err != nil {
		if isNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// memHGetInt reads a hash field from MemStore as int64.
// Returns -1 (not a valid count) when the key or field does not exist.
func memHGetInt(ctx context.Context, store *kv.Mem, key, field string) (int64, error) {
	b, err := store.HGet(ctx, key, field)
	if err != nil {
		if isNotFound(err) {
			return -1, nil
		}
		return 0, err
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// memHSetInt writes a hash field to MemStore as a decimal string.
func memHSetInt(ctx context.Context, store *kv.Mem, key, field string, val int64, ttl time.Duration) error {
	if err := store.HSet(ctx, key, field, []byte(strconv.FormatInt(val, 10)), ttl); err != nil {
		return err
	}
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, kv.ErrNotFound)
}

// memCommitImpl is the Go emulator for commitLuaScript.
func memCommitImpl(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
	if len(args) < 10 {
		return nil, fmt.Errorf("limit.commit: expected 10 args, got %d", len(args))
	}
	tok, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("limit.commit: arg[0] must be string")
	}
	guardTTLMs, err := toInt64(args[1])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[1] guard_ttl_ms: %w", err)
	}
	nCon, err := toInt64(args[2])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[2] n_con: %w", err)
	}
	nTok, err := toInt64(args[3])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[3] n_tok: %w", err)
	}
	tokAmountsJSON, ok2 := args[4].(string)
	if !ok2 {
		return nil, fmt.Errorf("limit.commit: arg[4] tok_amounts_json must be string")
	}
	tokTTLMs, err := toInt64(args[5])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[5] tok_ttl_ms: %w", err)
	}
	cancelledInt, err := toInt64(args[6])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[6] cancelled: %w", err)
	}
	cancelled := cancelledInt == 1
	tbRefundsJSON, ok3 := args[7].(string)
	if !ok3 {
		return nil, fmt.Errorf("limit.commit: arg[7] tb_refunds_json must be string")
	}
	lbRefundsJSON, ok4 := args[8].(string)
	if !ok4 {
		return nil, fmt.Errorf("limit.commit: arg[8] lb_refunds_json must be string")
	}
	swRefundsJSON, ok5 := args[9].(string)
	if !ok5 {
		return nil, fmt.Errorf("limit.commit: arg[9] sw_refunds_json must be string")
	}

	var tokAmounts []int64
	if err := json.Unmarshal([]byte(tokAmountsJSON), &tokAmounts); err != nil {
		return nil, fmt.Errorf("limit.commit: parse tok_amounts: %w", err)
	}
	// tb/lb refunds are JSON arrays-of-arrays: [[key_idx, cost, burst], ...]
	var tbRefunds [][3]int64
	if err := json.Unmarshal([]byte(tbRefundsJSON), &tbRefunds); err != nil {
		return nil, fmt.Errorf("limit.commit: parse tb_refunds: %w", err)
	}
	var lbRefunds [][2]int64
	if err := json.Unmarshal([]byte(lbRefundsJSON), &lbRefunds); err != nil {
		return nil, fmt.Errorf("limit.commit: parse lb_refunds: %w", err)
	}
	var swRefunds [][2]int64
	if err := json.Unmarshal([]byte(swRefundsJSON), &swRefunds); err != nil {
		return nil, fmt.Errorf("limit.commit: parse sw_refunds: %w", err)
	}

	guardKey := keys[0]
	guardTTL := time.Duration(guardTTLMs) * time.Millisecond
	tokTTL := time.Duration(tokTTLMs) * time.Millisecond

	if _, err := store.Get(ctx, guardKey); err == nil {
		return []byte("noop"), nil
	}

	for i := int64(1); i <= nCon; i++ {
		_, _ = store.Incr(ctx, keys[i], -1)
	}

	if !cancelled {
		for j := int64(0); j < nTok; j++ {
			var amount int64
			if int(j) < len(tokAmounts) {
				amount = tokAmounts[j]
			}
			if amount > 0 {
				keyIdx := 1 + nCon + j
				if _, err := store.Incr(ctx, keys[keyIdx], amount); err == nil && tokTTL > 0 {
					_ = store.Expire(ctx, keys[keyIdx], tokTTL)
				}
			}
		}
	}

	if cancelled {
		// tbRefunds: [key_idx, cost_scaled, burst]
		for _, entry := range tbRefunds {
			stateKey := keys[entry[0]-1]
			curI, _ := memHGetInt(ctx, store, stateKey, "tokens")
			if curI >= 0 {
				newT := curI + entry[1]
				if newT > entry[2]*1000 {
					newT = entry[2] * 1000
				}
				_ = store.HSet(ctx, stateKey, "tokens", []byte(strconv.FormatInt(newT, 10)), 0)
			}
		}
		// lbRefunds: [key_idx, cost_scaled]
		for _, entry := range lbRefunds {
			stateKey := keys[entry[0]-1]
			curI, _ := memHGetInt(ctx, store, stateKey, "level")
			if curI >= 0 {
				newL := curI - entry[1]
				if newL < 0 {
					newL = 0
				}
				_ = store.HSet(ctx, stateKey, "level", []byte(strconv.FormatInt(newL, 10)), 0)
			}
		}
		// swRefunds: [key_idx, count]
		for _, entry := range swRefunds {
			stateKey := keys[entry[0]-1]
			curI, _ := memHGetInt(ctx, store, stateKey, "count")
			if curI >= 0 {
				newC := curI - entry[1]
				if newC < 0 {
					newC = 0
				}
				_ = store.HSet(ctx, stateKey, "count", []byte(strconv.FormatInt(newC, 10)), 0)
			}
		}
	}

	_ = store.Set(ctx, guardKey, []byte(tok), guardTTL)
	return []byte("ok"), nil
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
