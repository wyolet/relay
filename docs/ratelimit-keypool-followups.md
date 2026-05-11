# Deferred rate-limit + keypool architecture work

Set of related findings from the integration-test diagnosis on 2026-05-11.
None are blocking shipping. All should be tackled together before the next
rate-limit strategy or upstream-tier feature lands.

---

## 1. 2-phase optimistic Reserve protocol

**Status:** open, deferred. Revisit before adding any new rate-limit strategy.

## The problem

Current Reserve Lua processes rules in declaration order, mutating state as it goes:

```
for each rule:
  mutate state (INCRBY or HMSET)
  check post-mutation result
  if exceeded: rollback() and return
```

When a later rule fails, `rollback()` must undo every prior rule's mutation. Today the rollback list is split:
- `inc_req` / `inc_con` for INCRBY-style mutations
- `inc_state` for HMSET-style mutations (added after the bug fix in e115aa6)

Adding a new strategy means remembering to push into `inc_state` after every successful HMSET. We already shipped one bug from forgetting this for TB/LB/SW; the next new strategy is one missed `table.insert` away from repeating it.

## The proposed fix

Two-phase optimistic protocol — check everything first, mutate only if all pass:

```
-- Phase 1: read-only check
for each rule:
  read current state
  compute hypothetical post-state and decision
  if exceeded: return early (no mutations have happened)
  stash the computed post-state for phase 2

-- Phase 2: commit
for each rule:
  write the stashed post-state
```

No rollback needed, ever. Adding a strategy = write `check(state, cost) -> decision + new_state` and `apply(new_state)`. The wrapper code can't forget rollback because there's nothing to roll back.

## Trade-offs

**Pros:**
- Eliminates an entire bug class (the one we just patched).
- Cleaner separation of concerns per strategy.
- Easier to reason about: no mid-script partial state.

**Cons:**
- ~5–10 extra lines of Lua per strategy (carry phase-1 results to phase-2).
- TB/LB refill math has to be carried explicitly between phases. Trivially correct because both phases run in one atomic Lua script with the same `now_ms`.
- Marginal extra memory (computed states held until phase 2). Irrelevant in practice.

**Performance:** roughly neutral. The current "mutate then maybe rollback" does at most 2 ops per failed rule (mutate + reverse); 2-phase does 1 read + 1 write per successful rule + nothing on failure (after the failing rule). Wash, or 2-phase is faster on the failure path.

## Why we haven't done it

The current design grew organically — sliding-window's `INCRBY then check the post-counter` pattern was natural for the first strategy. Token-bucket/leaky-bucket/session-window inherited the same shape when they were added. Rollback bug got found and fixed; the structure works.

The 2-phase rewrite is pure refactor — no behavior change. Earns its keep when we add the next strategy, or if a third strategy ever turns out to need different rollback semantics.

## Action items

- [ ] Sketch the 2-phase Lua for one strategy (TB is the gnarliest because of refill); confirm the carry-state-between-phases pattern is clean.
- [ ] If yes, refactor all 5 strategies + Mem emulator in one commit.
- [ ] Drop `inc_state` tracking entirely from `rollback()`.
- [ ] Verify all existing tests (pkg/ratelimit unit + integration) pass unchanged.

## Tangential: rule ordering

Order doesn't affect correctness (script is atomic, rollback works). It affects:
- Which rule's error message returns first.
- Performance: cheap-and-likely-to-fail rules first means less wasted work.

We could sort rules automatically by an estimated cost/likelihood heuristic. Micro-optimization; not worth doing until profiling says otherwise.

---

## 2. `RemainingByMeter` is strategy-blind

**Status:** open, deferred. Blocks honest keypool weighting.

### The problem

`pkg/ratelimit/limiter.go:RemainingByMeter` always reads SW-style bucket keys (`bucketKey(scope, rule, cur/prev)`), regardless of the rule's strategy:

```go
// requests, tokens, tokens.X — sliding window
curKey := bucketKey(scope, rule, cur)
prevKey := bucketKey(scope, rule, prev)
curVal, _ := readCounter(ctx, l.store, curKey)
prevVal, _ := readCounter(ctx, l.store, prevKey)
rate = interpolatedRate(curVal, prevVal, frac)
```

For a token-bucket / leaky-bucket / fixed-window / session-window rule the SW
bucket keys are empty (state lives in `:tb:`, `:lb:`, `:fw:`, `:sw:`). So
`RemainingByMeter` returns `amount` (full capacity) for every non-SW rule.

### Why it matters

- Keypool's weighted-random selection uses `RemainingByMeter` to compute key
  weights. For 4 of 5 strategies the weights are uniform → no actual weighting.
- Without this fix, multi-key tenants get effectively random selection unless
  their RLs are sliding-window. Defeats the wedge.

### Fix shape

Make `RemainingByMeter` strategy-aware. Read the strategy-specific state and
compute remaining honestly:

- `fixed-window`: read fixed-window counter, remaining = amount - counter.
- `token-bucket`: read hash, refill against now, remaining = tokens.
- `leaky-bucket`: read hash, drain against now, remaining = amount - level.
- `session-window`: read hash, if window expired remaining = amount, else
  amount - count.
- `sliding-window`: existing math.

~50 LOC + a contract test per strategy.

---

## 3. Keypool gate vs routing separation

**Status:** open, deferred. Caused integration test SW failure on 2026-05-11.

### The problem

`internal/keypool/keypool.go:Select` does two jobs that shouldn't be one:

1. **Selection**: pick which Secret (provider API key) to use.
2. **Gating**: return `ErrPoolOutOfCapacity` when all key weights sum to zero,
   which causes a 429 (`pool_out_of_capacity`) before `Reserve` ever runs.

The gate is a duplicate (and weaker) rate-limit decision. It uses
`RemainingByMeter` (today: SW-only, see §2 above) and rejects when the SW
counter hits the cap. But `Reserve` uses `>` not `>=`, so it would have
allowed one more request. The gate fires off-by-one early.

Worse, for non-SW strategies the gate never fires at all (because
`RemainingByMeter` always reports full capacity for them — see §2). So the
gate is *only* active for SW rules, and is *wrong* for them.

### Why it matters

- SW rules: every Nth request is rejected with the wrong error code one
  request before our real limit. Customers see `pool_out_of_capacity` instead
  of `rpm_exceeded`, with the wrong Retry-After computed from the wrong
  math.
- TB/LB/FW/SW rules: the gate doesn't fire. The keypool is "transparent" for
  them — fine today, but if we ever rely on the gate for upstream protection,
  it'd silently no-op.

### Fix shape

Make keypool **routing only**. Drop `ErrPoolOutOfCapacity` entirely. If all
weights are zero, just pick (round-robin among healthy, or first healthy by
prioritized order — see §4). Let `Reserve` be the single authority for
"is this request allowed."

When *all keys are truly drained* (Reserve returns `ExceededError` for every
healthy key in cooldown), emit a different error: `all_keys_exhausted` with
a Retry-After that's the minimum across all keys' cooldowns. Distinct from
single-key rate-limit because it tells the customer "add more keys or wait."

This is also a precondition for §5 (configurable key selection): prioritized
selection only works when keypool isn't gating.

---

## 4. Configurable key-selection strategy

**Status:** open, deferred. Real product feature.

### The problem

Keypool selection is hardcoded to "weighted random by remaining quota with
round-robin fallback." Customers want different policies:

- Drain key 1 fully before touching key 2 (most-requested pattern: paid key +
  free-tier overflow keys; or primary + backup).
- Even round-robin across all keys (predictable, doesn't trust weighting math).
- Weighted by tokens remaining (token-heavy workloads).
- LRU (fairness over time).

### Fix shape

Add `policy.spec.keySelection: prioritized | round-robin | weighted-quota`
(default `weighted-quota` = current behavior).

Per-policy because keypool is per-policy. If you want different selection
for different keys, use two policies.

Implementation: `internal/keypool/keypool.go:Select` becomes a dispatcher.
- `prioritized`: iterate `policy.spec.secrets[]` in declared order, return
  first healthy + non-cooldown one.
- `round-robin`: already exists as fallback; promote to strategy.
- `weighted-quota`: existing impl, but now using strategy-aware
  `RemainingByMeter` (§2).

~60 LOC + validation + tests.

Ship `prioritized` + `round-robin` + `weighted-quota` in v1. Defer LRU.

---

## 5. Wire our own ExceededError into the keypool circuit breaker

**Status:** open, deferred. Prerequisite for §4 `prioritized` to work cleanly.

### The problem

`secret_health` keys exist for circuit-breaking. Today they're only flipped
when *upstream* returns 429 (or other classified failure). When *our own*
Reserve returns `ExceededError` for a secret, that secret stays "healthy"
from keypool's POV — it'd be selected again on the next request, fail Reserve
again, return 429 again. Pointless churn.

This breaks `prioritized` selection: key 1 hits its rate-limit cap, every
subsequent request still picks key 1, every request gets 429 instead of
falling over to key 2.

### Fix shape

When `pipeline.Reserve` returns `ExceededError`, propagate the failure to
`secret_health` for the secret that was selected:

- State: `cooldown`
- `open_until`: now + `exceeded.RetryAfter`
- Reason: `local_rate_limit_exceeded` (distinguish from upstream 429)

Keypool's existing health check skips cooldown'd secrets. Next request: §4
selects the next healthy secret. Cooldown expires: secret becomes selectable
again.

Tiny change in `internal/pipeline/pipeline.go` around line 196 (the
`ExceededError` handling site). Maybe 20 LOC + a test.

---

## 6. System-mirrored upstream-tier rate limits

**Status:** declared but not implemented. Aspirational; biggest wedge feature.

### The problem

Providers have hard rate limits (OpenAI tier 1 = 500 RPM, tier 3 = 10k RPM, …).
Today Relay can't mirror them — it just proxies and lets the upstream 429
bubble back. That means:

- Customer 429s are slow (full upstream round-trip).
- Upstream's per-IP/per-key limit cuts unevenly across requests.
- We waste upstream quota on requests we'd just get 429'd for.

LiteLLM and OpenRouter both have this problem. Preempting it is the
infra-grade wedge.

### Fix shape

Three pieces:

1. `provider.spec.tier` (e.g. `"openai-tier-3"`) maps to a static table of
   known limits. Or operator declares them explicitly.
2. Snapshot loader auto-injects system-mirrored RateLimits attached to each
   Secret based on its Provider's tier. These get
   `metadata.name = "system-mirror-<provider>-<tier>"` and
   `spec.source = system_mirrored`.
3. Admin handlers reject PUT/DELETE on `source: system_mirrored` resources
   with 403 — operator can't accidentally break them.

The `RateLimitSource` const (`user_defined` / `system_mirrored`) and
`spec.source` field already exist (declared but unused). Field plumbing is
done; the auto-injection + admin guard is the work.

When implemented, the request flow naturally protects upstream:

```
ResolvedRules = policy.RLs + secret.RLs (user) + secret.RLs (system-mirrored) + model.RLs
                  ↓
                Reserve (one atomic Lua pass)
                  ↓
        all pass → call upstream
   system-mirror fails → 429 to customer, upstream never touched
```

Maybe 1 week of work. High product value.

---

## How they connect

```
                  ┌─────────────────────────────────┐
                  │ §6 system-mirrored RLs added to │
                  │   policy/secret attachments     │
                  └─────────────────────────────────┘
                                  │
                                  ▼
                  ┌─────────────────────────────────┐
                  │ §1 2-phase Reserve: cleaner     │
                  │   per-strategy code, no         │
                  │   rollback bugs as strategies   │
                  │   evolve                        │
                  └─────────────────────────────────┘
                                  │
                                  ▼
                  ┌─────────────────────────────────┐
                  │ §2 strategy-aware Remaining     │
                  │   so keypool can weight rules   │
                  │   from any strategy             │
                  └─────────────────────────────────┘
                                  │
                                  ▼
       ┌────────────────────────────────────────────────────┐
       │ §3 keypool routing-only + §4 selection strategies  │
       │ + §5 circuit-break on local exceed                 │
       │                                                    │
       │ → Reserve is the only authority for rate-limit     │
       │ → Keypool just answers "which key to use next"     │
       │ → all_keys_exhausted is its own distinct error     │
       └────────────────────────────────────────────────────┘
```

§1, §2, §3, §4, §5 together: ~250 LOC across 4 files, plus tests.
§6 separately: ~1 week.

## Suggested order

1. §2 (strategy-aware Remaining) — small, foundational, no behavior change.
2. §3 + §5 + §4 together — they're one cohesive keypool refactor.
3. §1 (2-phase Reserve) — before the next strategy.
4. §6 (system-mirrored) — once the rest is solid, ship the wedge feature.
