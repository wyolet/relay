# KV Migration Audit — `pkg/state` → `pkg/kv`

> Generated: 2026-05-07
> Scope: full inventory of `pkg/state` usage for Redis Cluster hash-tag migration.
> Status: READ-ONLY audit — no code changed.

---

## 1. `pkg/limit`

**Files:** `pkg/limit/limit.go`, `pkg/limit/scripts.go`, `pkg/limit/window.go`, `pkg/limit/keys.go`

### 1.1 KV operations used

| Operation | File:Line | Notes |
|-----------|-----------|-------|
| `RunScript("limit.reserve", …)` | `limit.go:96` | Single Lua call; KEYS = all counter keys for the request |
| `RunScript("limit.commit", …)` | `limit.go:166` | Single Lua call; KEYS = guard + concurrency + token bucket keys |
| `store.Get` | `window.go:33` (`readCounter`) | Read-only path via `RemainingByMeter` |
| `store.Incr` | `scripts.go:294,303,…` | Inside `memReserveImpl` / `memCommitImpl` (MemStore emulators only) |
| `store.Expire` | `scripts.go:297` | Inside `memReserveImpl` (MemStore emulator only) |
| `store.Set` | `scripts.go:492` | Inside `memCommitImpl` (MemStore emulator only) |
| `store.WithLock` | `scripts.go:282` | Inside `memReserveImpl` to emulate Lua atomicity on MemStore |

### 1.2 What is stored

#### Sliding-window request/token bucket counter
- **Key format:** `limit:<parentKind>:<parentName>:<rlName>:<meter>:<bucketTS>`
  - `keys.go:12–18` (`bucketKey`)
  - Example: `limit:Pool:prod-pool:per-minute:requests:2024-01-15T10:00:00Z`
  - `<parentKind>` ∈ `{Secret, Pool, Model}` (`configstore/snapshot.go:155,164,173`)
  - `<parentName>` = resource name string
  - `<rlName>` = `RateLimit.Metadata.Name`
  - `<meter>` ∈ `{requests, tokens}`
  - `<bucketTS>` = RFC3339 truncated to window boundary
- **Two slots per rule per call:** current bucket and previous bucket (sliding window)

#### Concurrency counter
- **Key format:** `limit:<parentKind>:<parentName>:<rlName>:<meter>`
  - `keys.go:23–29` (`concurrencyKey`)
  - Example: `limit:Secret:sk-abc123:max-concurrent:concurrency`

#### Commit idempotency guard
- **Key format:** `limit:committed:<reservationID>`
  - `keys.go:32–34` (`commitGuardKey`)
  - `<reservationID>` = `reqid.Generate()` (random UUID-like)

### 1.3 TTLs

| Key category | TTL |
|---|---|
| Sliding-window bucket (requests/tokens) | `window * 2` — set via `PEXPIRE` in Lua (`scripts.go:49`) |
| Concurrency counter | `window * 5` — set via `PEXPIRE` in Lua (`scripts.go:89`) |
| Commit guard | `5 * time.Minute` — constant `commitGuardTTL` (`limit.go:21`) |

### 1.4 Atomicity boundary

#### `limit.reserve` Lua script (`scripts.go:30–128`)
- **All keys touched in one EVAL:** every counter key for every rule active for the current request.
- Key set spans: current + previous bucket for each requests/token rule, plus concurrency key for each concurrency rule.
- **What the keys have in common:** they all belong to the same *incoming API request* — same pool, same secret, same model, resolved at request time from `cfg.RateLimitsForRequest(provider, pool, model, secret)`.
- The `parentKind` dimension means a single request may touch keys belonging to different parent kinds (`Secret`, `Pool`, `Model`) simultaneously, e.g.:
  - `limit:Secret:sk-abc:req-per-min:requests:2024-…`
  - `limit:Pool:prod-pool:pool-limit:requests:2024-…`
  - `limit:Model:gpt-4o:model-cap:tokens:2024-…`
- **All three keys must land on the same shard for the Lua script to be valid.** This is the critical constraint.

#### `limit.commit` Lua script (`scripts.go:144–175`)
- **Keys touched:** `[guardKey, ...conKeys, ...tokCurKeys]` (`limit.go:147–159`)
- Same multi-parent-kind spread as Reserve.
- Guard key is scoped to `reservationID`, not any resource — lives in a separate namespace.

#### MemStore emulator `memReserveImpl` (`scripts.go:252–416`)
- Uses `store.WithLock(ctx, keys, …)` at `scripts.go:282` — same key set as the Lua path.

### 1.5 Proposed hash tag

**Problem:** A single Lua script touches keys from up to three different parent kinds/names. There is no single resource identity that is common to all of them.

**Solution:** Use the **request's pool name** as the shard anchor. Every rule resolved for a request is associated with a pool (secrets belong to pools, models are routed through pools). All keys touched in a single Lua call will share `{pool:<poolName>}`.

> Caveat: model-scoped rules (`parentKind=Model`) are pool-agnostic in the data model, but in practice they are always evaluated in the context of a pool selection. Embedding `{pool:<poolName>}` into model-scoped keys creates a denormalized copy per pool — acceptable since model keys are already partitioned by bucket timestamp and are short-lived.

**Proposed hash tag:** `{pool:<poolName>}`

### 1.6 Proposed key formats

| Category | Migrated key |
|---|---|
| Bucket (request/token) | `limit:{pool:prod-pool}:Secret:sk-abc:per-min:requests:2024-01-15T10:00:00Z` |
| Bucket (model-scoped) | `limit:{pool:prod-pool}:Model:gpt-4o:model-cap:tokens:2024-01-15T10:00:00Z` |
| Concurrency | `limit:{pool:prod-pool}:Secret:sk-abc:max-con:concurrency` |
| Commit guard | `limit:{pool:prod-pool}:committed:<reservationID>` |

The pool name must be threaded through `buildReserveArgs` and `Commit` — requires adding `poolName string` to the key functions in `pkg/limit/keys.go`.

---

## 2. `pkg/keypool`

**Files:** `pkg/keypool/keypool.go`, `pkg/keypool/state.go`

### 2.1 KV operations used

| Operation | File:Line | Notes |
|-----------|-----------|-------|
| `state.Get` | `keypool.go:83` (`readRecord`) | Read circuit-breaker record |
| `state.Set` | `keypool.go:103` (`writeRecord`) | Write circuit-breaker record |
| `state.WithLock(…, rrKey)` | `keypool.go:224` | Lock around round-robin counter increment |
| `state.Incr(…, rrKey)` | `keypool.go:226` | Increment round-robin counter inside lock |

No `RunScript` usage — `pkg/keypool` does not use Lua.

### 2.2 What is stored

#### Circuit-breaker record
- **Key format:** `secret_health:<keyHash>`
  - `keypool.go:47` (constant `stateKeyPrefix = "secret_health:"`)
  - `keypool.go:78` (`stateKey` method)
  - `<keyHash>` = `Secret.KeyHash` (pre-computed hash of the raw API key — `configstore/types.go`)
  - Value: JSON-encoded `circuitRecord` (`state.go:7–16`)

#### Round-robin counter
- **Key format:** `pool_rr:<poolName>`
  - `keypool.go:48` (constant `rrKeyPrefix = "pool_rr:"`)
  - `keypool.go:79` (`rrKey` method)
  - `<poolName>` = `Pool.Metadata.Name`
  - Value: integer counter (via `state.Incr`)

### 2.3 TTLs

| Key category | TTL |
|---|---|
| Circuit-breaker record (non-indefinite) | `24 * time.Hour` — constant `ttlFlat` (`keypool.go:52`) |
| Circuit-breaker record (auth failure, `Indefinite=true`) | **none / no expiry** (`keypool.go:99–101`) |
| Round-robin counter | **none** — `Incr` has no TTL; `Expire` never called on `rrKey` |

### 2.4 Atomicity boundary

#### `WithLock` around round-robin (`keypool.go:224–229`)
- Locks and increments a single key: `pool_rr:<poolName>`
- All-or-nothing: 1 key, 1 pool — trivially cluster-safe.

#### `readRecord` / `writeRecord`
- Individual `Get`/`Set` calls — not atomic with each other.
- Race condition acknowledged in code comment (`keypool.go:113–114`): "Concurrent Picks may both pick the same half-open key; the caller's RecordSuccess/RecordFailure resolves the outcome."

### 2.5 Proposed hash tag

Circuit-breaker keys are per-secret (`keyHash`). They are never touched in a multi-key atomic operation, so sharding is unconstrained. Use `{secret:<keyHash>}` for semantic clarity and to group circuit and quota state together if quota keys are ever added.

Round-robin key is per-pool. Use `{pool:<poolName>}`.

**Proposed hash tags:**
- Circuit-breaker record: `{secret:<keyHash>}`
- Round-robin counter: `{pool:<poolName>}`

### 2.6 Proposed key formats

| Category | Migrated key |
|---|---|
| Circuit-breaker record | `secret_health:{secret:sha256abc123}` |
| Round-robin counter | `pool_rr:{pool:prod-pool}` |

---

## 3. `cmd/relay`

**File:** `cmd/relay/main.go`

### 3.1 Role
`cmd/relay/main.go:31` imports `pkg/state` only to construct the store (`state.NewRedis` / `state.New`) and pass `state.Store` to `limit.New` and `keypool.New`. No keys are constructed here.

### 3.2 KV operations used
None directly. Consumers (operations on keys) are entirely within `pkg/limit` and `pkg/keypool`.

### 3.3 Migration impact
- `state.RedisConfig` / `state.NewRedis` / `state.New` → becomes `kv.RedisConfig` / `kv.NewRedis` / `kv.NewMem`.
- `state.Store` interface reference at `main.go:231,247` → `kv.Store`.
- The package rename is mechanical.

---

## Cross-Consumer Atomicity Check

**No cross-consumer atomic operations exist.**

- `pkg/limit`'s Lua scripts (`limit.reserve`, `limit.commit`) touch only `limit:*` and `limit:committed:*` keys.
- `pkg/keypool`'s `WithLock` touches only `pool_rr:*` keys.
- `pkg/keypool`'s `Get`/`Set` touch only `secret_health:*` keys.
- No single Lua script or `WithLock` call touches keys from two different consumers.

**STATUS: CLEAR — no cross-consumer atomicity problems.**

---

## Anti-Pattern Findings

### AP-1 — Key construction outside `keys.go`

| Location | Issue |
|---|---|
| `keypool.go:78` (`stateKey`) | Key prefix `"secret_health:"` defined as constant at `keypool.go:47`; key constructor inlined as a one-liner method. Acceptable but not in a `keys.go`-style file. |
| `keypool.go:79` (`rrKey`) | Same pattern — `"pool_rr:"` constant and method in `keypool.go`. |
| `limit/keys.go` | GOOD — all three key functions (`bucketKey`, `concurrencyKey`, `commitGuardKey`) centralised here. |

Recommendation: move `stateKey` and `rrKey` constants + constructors into a new `pkg/keypool/keys.go` to match the `pkg/limit` pattern.

### AP-2 — Keys without TTL that should have one

| Key | Issue |
|---|---|
| `pool_rr:<poolName>` | No TTL ever set. The counter is purely a modular index (`idx % len(healthy)`), so staleness is harmless, but it accumulates indefinitely. Low-risk; still worth adding a long TTL (e.g. 30 days) to allow Redis to reclaim keys from deleted pools. |
| `secret_health:<keyHash>` (Indefinite auth failures) | Intentionally no expiry — operator must intervene. Document this explicitly in the struct and admin API. |

### AP-3 — Key collision risk

- `limit:Pool:prod-pool:…` vs `limit:Secret:prod-pool:…` — if a Secret and a Pool share the same name **and** the same rate-limit rule name, their keys collide. Unlikely in practice but possible. The `<parentKind>` segment prevents this — e.g. `limit:Pool:…` vs `limit:Secret:…` differ. **No collision.** ✓
- `limit:committed:<reservationID>` and `limit:<parentKind>:…` share the `limit:` prefix but differ at the third segment (`committed` vs a Kind string). **No collision.** ✓
- `secret_health:` and `pool_rr:` namespaces are disjoint. ✓

### AP-4 — `state.Range` with prefix scan (`redis.go:107`)

`Range` uses `SCAN` which is O(N) and cursor-based. It is not called on the hot path (no callers in `pkg/limit` or `pkg/keypool`), but the implementation will break silently in Cluster mode if the prefix spans multiple shards. Flag for removal or replacement with per-key `MGET` once hash tags are applied.

---

## Migration Risk Notes

### R-1 — `limit.reserve` and `limit.commit` span multiple hash-tag candidates

The most dangerous aspect of this migration: a single Lua `EVAL` call touches keys whose `parentKind` varies (`Secret`, `Pool`, `Model`). All those keys must map to the same Redis Cluster slot. The proposed `{pool:<poolName>}` embedding into all key formats achieves this, but it requires:

1. `buildReserveArgs` (`scripts.go:204`) to accept `poolName` and pass it through to `bucketKey` / `concurrencyKey`.
2. `Commit` (`limit.go:141`) to accept or derive `poolName` and pass it to `bucketKey`.
3. `Reservation` struct (`limit.go:48`) to store `poolName` so `Commit` can use it.
4. `RemainingByMeter` (`limit.go:192`) to also accept `poolName` for the read path (consistency).

### R-2 — Commit guard key must move to same shard as concurrency/token keys

Current: `limit:committed:<reservationID>` has no pool prefix — it will hash to an arbitrary slot. In Cluster mode the `commitLuaScript` touches `KEYS[1]` (guard) and `KEYS[2+]` (concurrency/token keys) in the same EVAL. If the guard hashes to a different slot, Redis will return a `CROSSSLOT` error.

Fix: `commitGuardKey` must embed the pool hash tag: `limit:{pool:<poolName>}:committed:<reservationID>`.

### R-3 — `RedisConfig` has no Cluster mode

`pkg/kv/redis.go` uses `redis.NewClient` (single) or `redis.NewFailoverClient` (sentinel). Neither is a `redis.NewClusterClient`. The rename PR must add a `ClusterAddrs []string` field and branch to `redis.NewClusterClient` when set. The `go-redis/v9` `UniversalClient` already abstracts this, but `RedisConfig` does not expose cluster addresses.

### R-4 — `Range` / `SCAN` is incompatible with Cluster mode

`Redis.Range` (`redis.go:106–146`) uses `SCAN` against a single node. In Cluster mode, `SCAN` only covers one shard's keyspace. `Range` has no callers today but its presence is a latent hazard — mark with a `// TODO(kv): Cluster-unsafe` comment and file a follow-up to remove or shard-aware-ify it.

### R-5 — `WithLock` uses multi-key `KEYS` in Lua (`redis.go:149–166`)

The `luaAcquire` script takes N keys (sorted, deduped). In Cluster mode all N keys must hash to the same slot. The only caller is `keypool.go:224` with a single key (`rrKey`), so this is currently safe. But the generic multi-key path could be misused post-migration. Add a `// Cluster safety: all keys must share the same hash tag` comment in `WithLock`.

### R-6 — `kv.ErrNotFound` string comparison in `scripts.go`

`scripts.go:435`: `err.Error() == kv.ErrNotFound.Error()` — this is fragile string comparison instead of `errors.Is(err, kv.ErrNotFound)`. Fix during the hash-tag PR to avoid silent breakage if the error message changes.

### R-7 — `pool_rr` counter has no TTL

See AP-2. After the rename PR, add a 30-day TTL on creation. The counter semantics are unaffected (modular index).

### R-8 — `secret_health` keys with `Indefinite=true` will accumulate forever

No sweep mechanism exists. If provider keys are deleted from the catalog, their `secret_health:` records remain in Redis. Post-migration, a cleanup job or TTL-on-write policy for deleted secrets should be added.
