package limit

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
//   KEYS  = all counter keys, positionally matched to rules_json entries.
//           Each rule entry references its key by 1-based index into KEYS
//           (cur_key_idx, prev_key_idx for requests/tokens; con_key_idx for concurrency).
//   ARGV[1] = now_ms  (int)
//   ARGV[2] = rules_json (JSON array of ruleArg)
//   ARGV[3] = token (unique reservation ID, stored in the result)
//
// Returns JSON-encoded reserveResult.
//
// Sliding-window math (same as Go fractionElapsed / interpolatedRate):
//   rate = cur + prev * (1 - elapsed/window)
// where elapsed = now_ms % window_ms.
const reserveLuaScript = `
-- Reserve: atomic sliding-window check + increment.
-- ARGV[1]=now_ms, ARGV[2]=rules_json, ARGV[3]=token
local now_ms   = tonumber(ARGV[1])
local rules    = cjson.decode(ARGV[2])
local tok      = ARGV[3]

-- track which concurrency keys we incremented so we can roll back.
local inc_con  = {}  -- list of {key, ttl_ms}
local inc_req  = {}  -- list of {key, ttl_ms} for requests (rollback on failure)

for i, r in ipairs(rules) do
  local meter = r.meter
  local amount = tonumber(r.amount)
  local win_ms = tonumber(r.window_ms)

  if meter == "requests" then
    local cur_key  = KEYS[r.cur_key_idx]
    local prev_key = KEYS[r.prev_key_idx]
    local ttl_ms   = win_ms * 2

    local new_cur = redis.call('INCRBY', cur_key, 1)
    redis.call('PEXPIRE', cur_key, ttl_ms)

    local prev_raw = redis.call('GET', prev_key)
    local prev = prev_raw and tonumber(prev_raw) or 0

    -- elapsed fraction into the current bucket [0,1]
    local bucket_start_ms = math.floor(now_ms / win_ms) * win_ms
    local elapsed_ms = now_ms - bucket_start_ms
    local frac = elapsed_ms / win_ms

    -- sliding rate: current + prev weighted by time remaining from prior window
    local rate = new_cur + prev * (1.0 - frac)
    if rate > amount then
      -- rollback this increment
      redis.call('INCRBY', cur_key, -1)
      -- rollback all prior increments
      for _, v in ipairs(inc_req) do redis.call('INCRBY', v[1], -1) end
      for _, v in ipairs(inc_con) do redis.call('INCRBY', v[1], -1) end
      -- compute retry_after: wait until prev*(1-frac) drops enough
      -- need: (new_cur-1) + prev*(1-frac_t) < amount  => frac_t > 1 - (amount-(new_cur-1))/prev
      local retry_ms = win_ms - elapsed_ms  -- safe upper bound
      if prev > 0 then
        local need = (amount - (new_cur - 1)) / prev
        local frac_t = 1.0 - need
        if frac_t > 0 and frac_t < 1.0 then
          local target_ms = frac_t * win_ms
          local wait = target_ms - elapsed_ms
          if wait > 0 and wait < retry_ms then retry_ms = wait end
        end
      end
      return cjson.encode({exceeded=true, retry_after_ms=math.floor(retry_ms),
        parent_kind=r.parent_kind, parent_name=r.parent_name, rule_name=r.rule_name, meter=meter})
    end
    table.insert(inc_req, {cur_key, ttl_ms})

  elseif meter == "concurrency" then
    local con_key = KEYS[r.con_key_idx]
    local ttl_ms  = win_ms * 5

    local new_val = redis.call('INCRBY', con_key, 1)
    redis.call('PEXPIRE', con_key, ttl_ms)

    if new_val > amount then
      redis.call('INCRBY', con_key, -1)
      for _, v in ipairs(inc_req) do redis.call('INCRBY', v[1], -1) end
      for _, v in ipairs(inc_con) do redis.call('INCRBY', v[1], -1) end
      return cjson.encode({exceeded=true, retry_after_ms=win_ms,
        parent_kind=r.parent_kind, parent_name=r.parent_name, rule_name=r.rule_name, meter=meter})
    end
    table.insert(inc_con, {con_key, ttl_ms})

  elseif meter == "tokens" then
    -- tokens: peek only at Reserve time; Commit will increment.
    local cur_key  = KEYS[r.cur_key_idx]
    local prev_key = KEYS[r.prev_key_idx]

    local cur_raw  = redis.call('GET', cur_key)
    local prev_raw = redis.call('GET', prev_key)
    local cur_val  = cur_raw and tonumber(cur_raw) or 0
    local prev_val = prev_raw and tonumber(prev_raw) or 0

    local bucket_start_ms = math.floor(now_ms / win_ms) * win_ms
    local frac = (now_ms - bucket_start_ms) / win_ms
    local rate  = cur_val + prev_val * (1.0 - frac)
    if rate >= amount then
      for _, v in ipairs(inc_req) do redis.call('INCRBY', v[1], -1) end
      for _, v in ipairs(inc_con) do redis.call('INCRBY', v[1], -1) end
      local retry_ms = win_ms - (now_ms - bucket_start_ms)
      return cjson.encode({exceeded=true, retry_after_ms=math.floor(retry_ms),
        parent_kind=r.parent_kind, parent_name=r.parent_name, rule_name=r.rule_name, meter=meter})
    end
    -- no table entry — tokens not incremented at reserve
  end
end

return cjson.encode({exceeded=false, token=tok})
`

// commitLuaScript is the atomic Commit script.
//
// Wire format:
//   KEYS[1]     = guard key (idempotency: limit:committed:<token>)
//   KEYS[2..N]  = concurrency keys to decrement
//   KEYS[N+1..] = token bucket keys (cur key per token rule)
//   ARGV[1]     = reservation token (for guard value)
//   ARGV[2]     = guard_ttl_ms
//   ARGV[3]     = n_con  (number of concurrency keys)
//   ARGV[4]     = n_tok  (number of token rule keys)
//   ARGV[5]     = actual_tokens (int64, 0 means skip token increment)
//   ARGV[6]     = tok_ttl_ms (2*window of token rule, or 0 to skip)
//
// Returns "ok" or "noop" (duplicate).
const commitLuaScript = `
-- Commit: decrement concurrency + post-hoc token increment. Idempotent.
local guard_key   = KEYS[1]
local tok         = ARGV[1]
local guard_ttl   = tonumber(ARGV[2])
local n_con       = tonumber(ARGV[3])
local n_tok       = tonumber(ARGV[4])
local actual_tok  = tonumber(ARGV[5])
local tok_ttl_ms  = tonumber(ARGV[6])

-- idempotency: if guard already set, this is a duplicate commit
if redis.call('EXISTS', guard_key) == 1 then
  return "noop"
end

-- decrement concurrency counters (always, even on cancel)
for i = 2, 1 + n_con do
  redis.call('INCRBY', KEYS[i], -1)
end

-- increment token buckets post-hoc (only if actual_tok > 0)
if actual_tok > 0 then
  for i = 2 + n_con, 1 + n_con + n_tok do
    redis.call('INCRBY', KEYS[i], actual_tok)
    redis.call('PEXPIRE', KEYS[i], tok_ttl_ms)
  end
end

-- set guard to prevent duplicate commits
redis.call('SET', guard_key, tok, 'PX', guard_ttl)
return "ok"
`

// ruleArg is the JSON shape passed to the Reserve Lua script per rule.
type ruleArg struct {
	Meter      string `json:"meter"`
	Amount     int64  `json:"amount"`
	WindowMs   int64  `json:"window_ms"`
	ParentKind string `json:"parent_kind"`
	ParentName string `json:"parent_name"`
	RuleName   string `json:"rule_name"`
	// key indices (1-based into KEYS slice)
	CurKeyIdx int `json:"cur_key_idx,omitempty"`
	PrevKeyIdx int `json:"prev_key_idx,omitempty"`
	ConKeyIdx  int `json:"con_key_idx,omitempty"`
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

// buildReserveArgs builds (keys, ruleArgs, keyIndex) for the Reserve script.
// Returns sorted KEYS slice and ruleArg slice. keyIndex maps key→1-based index.
// poolName is embedded as a hash tag in every key so all keys in a single EVAL
// call map to the same Redis Cluster slot.
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
		w := rule.RateLimit.Spec.Window
		cur, prev := windowBuckets(now, w)
		ra := ruleArg{
			Meter:      string(rule.Meter),
			Amount:     rule.RateLimit.Spec.Amount,
			WindowMs:   w.Milliseconds(),
			ParentKind: string(rule.ParentKind),
			ParentName: rule.ParentName,
			RuleName:   rule.RateLimit.Metadata.Name,
		}
		switch rule.Meter {
		case catalog.MeterRequests, catalog.MeterTokens:
			ra.CurKeyIdx = addKey(bucketKey(poolName, rule, cur))
			ra.PrevKeyIdx = addKey(bucketKey(poolName, rule, prev))
		case catalog.MeterConcurrency:
			ra.ConKeyIdx = addKey(concurrencyKey(poolName, rule))
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
// It replicates the same sliding-window + rollback logic atomically using
// MemStore.WithLock (already held by the caller via RunScript's locking model;
// MemStore.RunScript does NOT hold a lock — we must take one inside).
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

	// Use WithLock to make the whole check-and-increment atomic (mirrors what
	// Lua atomicity gives us on Redis).
	lockErr := store.WithLock(ctx, keys, func(ctx context.Context) error {
		for _, r := range rules {
			amount := r.Amount
			winMs := r.WindowMs

			switch r.Meter {
			case "requests":
				curKey := keys[r.CurKeyIdx-1]
				prevKey := keys[r.PrevKeyIdx-1]
				ttl := time.Duration(winMs*2) * time.Millisecond

				newCur, err := store.Incr(ctx, curKey, 1)
				if err != nil {
					return err
				}
				_ = store.Expire(ctx, curKey, ttl)

				prevVal, err := memReadCounter(ctx, store, prevKey)
				if err != nil {
					return err
				}

				bucketStartMs := (nowMs / winMs) * winMs
				elapsedMs := nowMs - bucketStartMs
				frac := float64(elapsedMs) / float64(winMs)
				rate := float64(newCur) + float64(prevVal)*(1.0-frac)

				if rate > float64(amount) {
					_, _ = store.Incr(ctx, curKey, -1)
					for _, v := range incReq {
						_, _ = store.Incr(ctx, v.key, -1)
					}
					for _, v := range incCon {
						_, _ = store.Incr(ctx, v.key, -1)
					}
					retryMs := float64(winMs) - float64(elapsedMs)
					if prevVal > 0 {
						need := float64(amount-(newCur-1)) / float64(prevVal)
						fracT := 1.0 - need
						if fracT > 0 && fracT < 1.0 {
							targetMs := fracT * float64(winMs)
							wait := targetMs - float64(elapsedMs)
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
					for _, v := range incReq {
						_, _ = store.Incr(ctx, v.key, -1)
					}
					for _, v := range incCon {
						_, _ = store.Incr(ctx, v.key, -1)
					}
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

			case "tokens":
				curKey := keys[r.CurKeyIdx-1]
				prevKey := keys[r.PrevKeyIdx-1]

				curVal, err := memReadCounter(ctx, store, curKey)
				if err != nil {
					return err
				}
				prevVal, err := memReadCounter(ctx, store, prevKey)
				if err != nil {
					return err
				}

				bucketStartMs := (nowMs / winMs) * winMs
				elapsedMs := nowMs - bucketStartMs
				frac := float64(elapsedMs) / float64(winMs)
				rate := float64(curVal) + float64(prevVal)*(1.0-frac)

				if rate >= float64(amount) {
					for _, v := range incReq {
						_, _ = store.Incr(ctx, v.key, -1)
					}
					for _, v := range incCon {
						_, _ = store.Incr(ctx, v.key, -1)
					}
					retryMs := float64(winMs) - float64(elapsedMs)
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

func isNotFound(err error) bool {
	return errors.Is(err, kv.ErrNotFound)
}

// memCommitImpl is the Go emulator for commitLuaScript.
func memCommitImpl(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
	if len(args) < 6 {
		return nil, fmt.Errorf("limit.commit: expected 6 args, got %d", len(args))
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
	actualTok, err := toInt64(args[4])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[4] actual_tokens: %w", err)
	}
	tokTTLMs, err := toInt64(args[5])
	if err != nil {
		return nil, fmt.Errorf("limit.commit: arg[5] tok_ttl_ms: %w", err)
	}

	guardKey := keys[0]
	guardTTL := time.Duration(guardTTLMs) * time.Millisecond
	tokTTL := time.Duration(tokTTLMs) * time.Millisecond

	// Check idempotency guard.
	if _, err := store.Get(ctx, guardKey); err == nil {
		return []byte("noop"), nil
	}

	// Decrement concurrency.
	for i := int64(1); i <= nCon; i++ {
		_, _ = store.Incr(ctx, keys[i], -1)
	}

	// Increment token buckets post-hoc.
	if actualTok > 0 {
		for i := int64(1) + nCon; i < int64(1)+nCon+nTok; i++ {
			if _, err := store.Incr(ctx, keys[i], actualTok); err == nil && tokTTL > 0 {
				_ = store.Expire(ctx, keys[i], tokTTL)
			}
		}
	}

	// Set guard.
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
