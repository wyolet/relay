# pkg/kv — Developer Guide

Audience: engineers (and Claude when pointed at this file) writing new consumers of `pkg/kv`.

---

## 1. Purpose

`pkg/kv` is a generic key-value store abstraction with two backends: an in-process `Mem` backend (used in tests and local dev) and a `Redis` backend that is compatible with any RESP-speaking server (Redis, Valkey, Dragonfly). The package is a pure library — it contains no Relay business logic, no domain types, no knowledge of pools, routes, or API keys. Consumers such as `pkg/limit`, `internal/keypool`, and future packages like `internal/idempotency` and `internal/batch` build all domain semantics on top of the primitives `pkg/kv` exposes (get, set, delete, atomic Lua script execution, and distributed locking).

---

## 2. Why hash tags

Redis Cluster routes each key to a slot determined by a CRC16 hash of the key (or of the substring inside `{...}` if one is present). When a Lua script touches two keys that hash to different slots, Redis Cluster rejects the call with a `CROSSSLOT` error — the script cannot run atomically across nodes.

The fix is **hash tags**: if a key contains a `{tag}` substring, Redis uses only that substring to compute the slot. All keys sharing the same tag land on the same node and can be touched together in a single Lua script.

We adopt this convention **now**, even while running on a single Redis node, because:

- Migrating to Cluster later is operationally free if every key is already tagged.
- Migrating untagged keys is impossible to do atomically — you would have to drain traffic, rename keys, and restart; that window is unavoidable downtime.
- The discipline of choosing a tag forces the author to think about the atomicity boundary up front, which catches design mistakes before they calcify.

The rule is simple: **every key written through `pkg/kv` must begin with `{tag}:`**, where `tag` is the appropriate shard key for that consumer (see section 3).

---

## 3. Choosing a shard key

The shard key should equal the **atomicity boundary** — the smallest scope within which all keys a single Lua script or lock might touch must coexist.

| Consumer | Shard key | Rationale |
|---|---|---|
| `pkg/limit` (rate-limit buckets) | `{pool:NAME}` | `Reserve` atomically touches multiple bucket and meter keys for one pool in a single Lua call. All must be on the same slot. |
| `internal/keypool` (circuit breakers, round-robin counters) | `{pool:NAME}` | Shares the pool boundary with `pkg/limit` and may participate in a joint Lua call in the future (e.g., select key + decrement quota in one round trip). |
| `internal/idempotency` (future) | `{req:ID}` | Idempotency checks are per-request; no atomic op ever spans two different request IDs, so a request-scoped tag is the right granularity. |
| `internal/batch` (future) | `{job:ID}` | All state for a batch job (status, result pointer, retry counter) is job-scoped; no cross-job atomicity is needed. |
| Global counters | `{global}` | Last resort only. A global tag pins **all** such keys to a single shard forever. Acceptable for a handful of truly global counters; never for high-cardinality data. Document the cost in a code comment. |

When in doubt, pick the narrowest scope that covers your atomic operations.

---

## 4. Anti-patterns

**Do not put the hash tag in the middle or end of the key.**

```
# Wrong — Redis only honors the FIRST {…} in the key string.
# A tag at position > 0 works today on single-node Redis
# (because there is no slot routing) but silently breaks on Cluster.
"pool:NAME:{rl}:tokens"   # tag is not the prefix
```

**Do not include multiple `{...}` substrings in one key.**

```
# Wrong — Redis uses only the first match; the second is decoration
# that misleads readers into thinking it participates in routing.
"{pool:NAME}:cb:{key:ID}"
```

**Do not base the tag on variable-length or unbounded user input without sanitizing.**

Using raw user-supplied strings (API key identifiers, route names) directly in the tag is fine as long as the cardinality is bounded and the strings are validated. Avoid tags derived from request bodies or query parameters without explicit limits.

**Do not use untagged keys "for now".**

There is no safe migration path from untagged to tagged keys in a live system. An untagged key and its tagged replacement are different keys — you cannot rename them atomically. Starting without tags means permanently foregoing Cluster compatibility for that data, or accepting a cold-start migration.

**Do not build key strings inline at call sites.**

```
// Wrong
err = store.Set(ctx, fmt.Sprintf("{pool:%s}:tokens:%s", poolName, window), val, ttl)

// Right — call a builder defined in keys.go
err = store.Set(ctx, keys.TokenBucket(poolName, window), val, ttl)
```

Inline key construction scatters the naming convention across the codebase, makes typos silent, and prevents a single-place correctness assertion (see checklist item 6 in section 5).

---

## 5. Adding a new consumer — checklist

1. **Pick your shard key.** Identify the atomicity boundary (the set of keys that must coexist on one slot for your Lua scripts or locks to work). Name the tag accordingly.

2. **Create `keys.go` in your package.** Define one exported builder function per key category. Each builder returns a fully-formed key string beginning with `{tag}:`. No key string should appear anywhere else in the package.

3. **Declare a narrow interface.** Define a local interface in your package that lists only the `pkg/kv` methods your package actually calls. Import and use that interface, not a concrete `kv.Store` type. This makes dependencies explicit and keeps tests simple.

4. **Set TTLs on every key.** Pass a non-zero `time.Duration` to every `Set` or equivalent call. If a key is intentionally persistent, add a comment explaining why it has no TTL. (Hint: truly persistent state almost certainly belongs in Postgres, not in kv.)

5. **Write tests against both backends.** Your test suite must run the same table of cases against `kv.Mem` and `kv.Redis` (the latter via a test container or a local Redis instance). Backend-specific behavior (e.g., TTL precision) must be handled; functional behavior must be identical.

6. **Assert key format in a unit test.** Add a test that calls every builder in `keys.go` with representative inputs and asserts the output matches the regular expression `^\{[^}]+\}:`. This catches tag omissions and misplaced tags before they reach production.

7. **Document expected kv ops per request.** In the package-level doc comment (the `// Package ...` block at the top of the main `.go` file), state how many `pkg/kv` operations are expected per hot-path request. This makes regressions visible during review.

---

## 6. TTL guidelines

| Key type | Recommended TTL |
|---|---|
| Sliding-window rate-limit bucket | `window duration + small grace period` (e.g., window + 10 s) to tolerate clock skew without leaking state. |
| Distributed lock | Sane upper bound on the critical section, typically ≤ 30 s. Never unbounded. |
| Circuit-breaker record | Maximum half-open recovery window, typically ≤ 1 h. |
| Idempotency key | Exactly the dedupe window promised to the caller. |
| Cached config snapshot | Short (seconds to low minutes); the authoritative copy is in Postgres. |

**Persistent state belongs in Postgres.** If you find yourself wanting a kv key with no TTL because the data must survive a Redis flush, that is a signal the data should live in the control-plane store, not in `pkg/kv`.

---

## 7. What `pkg/kv` will not do

These are explicit non-goals. Do not add them to the package; solve them at the consumer layer or in a purpose-built package.

- **No auto-retry on `RunScript`.** Transient errors (e.g., `LOADING`, `TRYAGAIN`) are the consumer's responsibility to handle or bubble up. The hot path has an explicit retry budget; `pkg/kv` does not know it.
- **No transparent compression.** If a consumer stores large values and wants compression, it compresses before calling `Set` and decompresses after `Get`.
- **No schema validation on values.** `pkg/kv` stores and retrieves bytes/strings. Type safety is the consumer's concern.
- **No built-in key namespacing helpers beyond what the package already exposes.** Consumers own their key format and centralize it in `keys.go` as described above.
- **No transactions across multiple operations.** Use `RunScript` (Lua) when atomicity is required. There is no multi-step transaction facility.
- **No pub/sub.** Pub/sub is a separate concern. If a future consumer needs it, a dedicated `pkg/coord` (or similar) package is the right home, not `pkg/kv`.
