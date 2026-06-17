# Rate-Limit Strategies

Reference: https://smudge.ai/blog/ratelimit-algorithms

## Semantics table

| Strategy | Field meanings | State | Burst | Refund on cancel | Best for |
|---|---|---|---|---|---|
| `token-bucket` (**default**) | `amount` = burst capacity; `window` = refill period | Redis hash `{tokens, last_ms}` | Yes — up to `amount` | Yes | Bursty API clients with a sustained throughput ceiling |
| `sliding-window` | `amount` = max requests; `window` = rolling window width | Two counters (cur + prev bucket) | Soft — up to 2× amount at boundary | No | Smooth request distribution; no sudden resets |
| `fixed-window` | `amount` = max requests; `window` = bucket period | Single counter per floor(now/window) bucket | Hard at window boundary | No | Simple RPM/RPH caps with predictable reset time |
| `leaky-bucket` | `amount` = queue depth; `window` = drain period | Redis hash `{level, last_ms}` | Yes — up to `amount` | Yes | Constant-rate upstream protection |
| `session-window` | `amount` = max requests per session; `window` = session duration | Redis hash `{count, anchor_ms}` | No — hard count | Yes | Session-style quotas anchored to first activity (e.g. Anthropic's 5-hour limits) |

Concurrency meter (`meter: concurrency`) ignores strategy; it is always a gauge counter.

## Math

### Token bucket

```
refill_rate = amount / window_ms   (tokens / ms)

on reserve:
  tokens = min(amount, tokens + (now - last_ms) * refill_rate)
  if tokens < 1: throttle, retry_after = (1 - tokens) / refill_rate
  else: tokens -= 1

on cancel-commit:
  tokens = min(amount, tokens + 1)   (add back the deducted cost)
```

State stored with *1000 fixed-point integers to avoid Lua float precision issues.

### Leaky bucket (as-queue variant)

```
leak_rate = amount / window_ms   (per ms)

on reserve:
  level = max(0, level - (now - last_ms) * leak_rate)
  if level + 1 > amount: throttle, retry_after = (level + 1 - amount) / leak_rate
  else: level += 1

on cancel-commit:
  level = max(0, level - 1)
```

### Fixed window

```
bucket_start = floor(now_ms / window_ms) * window_ms
counter key  = ...fw:...<bucket_start>
INCR counter; if counter > amount: rollback, retry_after = window - (now - bucket_start)
```

### Sliding window (two-bucket interpolation)

```
rate = cur_bucket + prev_bucket * (1 - elapsed / window)
if rate > amount: throttle
```

### Session window (anchor-on-first)

```
state = {count, anchor_ms}

on reserve:
  if state.anchor_ms is unset or now >= anchor_ms + window:
    anchor_ms = now   # arm a new session
    count = 0
  count += 1
  if count > amount: throttle, retry_after = anchor_ms + window - now

on commit-cancel:
  count -= 1
```

The window does NOT reset on a wall-clock timer. It expires after `window` and then sits idle until a request arrives, which anchors a fresh window to that request. A user who is idle for 10× the window and then sends one request gets a clean slate at that moment, not on some calendar boundary.

## When to pick

**Token bucket** — default and recommended for most request-rate limits. Absorbs short bursts while enforcing a long-run average. Clients that hit the limit get a precise `retry_after`.

**Sliding window** — when you want a smooth rate with no cliff at window boundaries and don't mind slightly higher memory use (two counters). Use for token-meter rules where the 95th-percentile client is steady.

**Fixed window** — simplest to reason about. Reset time is predictable. Avoid when bursts at the window boundary are a concern (clients can double the limit by timing requests across the boundary).

**Leaky bucket** — when upstream capacity is fixed and you want to queue/smooth bursts rather than immediately reject them. The `amount` controls how much queuing is tolerated.

**Session window** — when limits should be anchored to user activity rather than a calendar clock. Matches the Anthropic "5-hour session" model: a quota that resets only after the window has fully elapsed *and* a new request arrives. Good for tiered usage where idle users shouldn't be penalized for elapsed clock time.

## Strategy is per-rule

Strategy lives on `RateLimitRule`, not on `RateLimitSpec`. A single RateLimit can mix strategies across rules:

```yaml
kind: RateLimit
spec:
  window: 1m
  rules:
    - meter: requests
      amount: 100
      strategy: token-bucket
    - meter: tokens
      amount: 1000000
      strategy: sliding-window
```

Legacy YAML/JSON that sets `strategy` at the spec level is still accepted: the value fans out to any rule that omits its own strategy field.

## Concurrency meter

`meter: concurrency` is a simple gauge (INCR on reserve, DECR on commit). Strategy is advisory and ignored at runtime. Set it to any valid value or omit it.

## Tokens meter constraint

`meter: tokens` and `meter: tokens.<suffix>` are **post-hoc** — the amount to charge is unknown at reserve time and arrives with the upstream response. Only `sliding-window` has a Commit-time increment path; the other strategies would silently no-op. Validation rejects `tokens` with `token-bucket` / `leaky-bucket` / `fixed-window`.

Use `sliding-window` for any tokens-based limit. Use the other strategies for `requests` (or the gauge `concurrency`).

## RateLimit as a group

One RateLimit object with multiple rules is the idiomatic way to express a tier. A single attachment from a Policy or Secret applies all rules at once:

```yaml
kind: RateLimit
metadata:
  name: tier-basic
spec:
  window: 1m
  rules:
    - meter: requests
      amount: 60
      strategy: token-bucket
    - meter: tokens
      amount: 100000
      strategy: sliding-window
    - meter: concurrency
      amount: 5
```

`window` is per-rule and falls back to `spec.window` when omitted. Different rules in the same RateLimit can carry different windows — useful for e.g. a daily token quota alongside a per-minute request rate.

## State TTLs

| Strategy | TTL |
|---|---|
| sliding-window | `window * 2` per bucket |
| fixed-window | `window * 2` |
| token-bucket | `window * 2` |
| leaky-bucket | `window * 2` |
| session-window | `window * 2` |
| concurrency | `window * 5` |

Stale keys expire automatically. No background job is needed; all strategies use lazy computation on read.
