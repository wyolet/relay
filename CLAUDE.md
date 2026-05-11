# CLAUDE.md

Guidance for Claude Code working in this repo.

## What this is

Wyolet Relay — a high-throughput LLM router in Go. Self-hostable, k8s-native, BYO provider keys. Competes with OpenRouter and LiteLLM on the infrastructure axis (performance, key pooling, batch orchestration, observability).

Full design docs live in the user's Obsidian vault at `~/Documents/Obsidian Vault/Projects/Relay/`. Read those before making non-trivial architectural suggestions.

## Wedge — what we are NOT

- Not a marketplace / reseller of provider tokens (deferred indefinitely)
- Not Python — performance is a wedge, runtime cost matters
- Not a drop-in clone of LiteLLM with a Go badge — feature parity isn't the goal, infra-grade throughput is
- Not adaptive / quality-aware routing in v1 — bad auto-routing loses customers
- Not a custom wire protocol — OpenAI/Anthropic shapes are accepted as passthrough

## Locked architectural decisions

The "spine" is the **Domain Model + Storage Layer**, not the request pipeline:

```
Domain & Storage:  Postgres (config) + Redis (hot/counters) + ClickHouse (events) + S3 (opt-in payloads)
Subsystems:        Realtime data plane | Batch workers | Control plane
```

- **Control plane** owns Postgres truth, publishes via pub/sub
- **Data plane** is stateless, bootstraps from Redis, never blocks on PG
- **Batch subsystem** reuses the realtime path's provider adapters and key-pool selector — do not fork

### Domain model
```
Org → Project → Route (named bundle: pool + policy + ACL + budget + storage)
              → ApiKey
              → KeyPool → ProviderKey
              → Budget
       Team / User (RBAC)
```

The **Route** is the unit of intent. Customer sends `X-Relay-Route: prod-cheap`; admins own the meaning. Header overrides are an escape hatch, not the primary interface.

### Identity model (every catalog resource)

Three-field identity, each with a distinct role:

- `metadata.id` — UUIDv7, **immutable**, server-stamped on create. The PG primary key. Used in all id-routed admin URLs.
- `metadata.name` — DNS-1123 slug, stable, mutable. Auto-derived from `displayName` on create with a collision suffix. Used in human-readable URLs and YAML refs.
- `metadata.displayName` — free text. Edits are free; nothing references it. Editing displayName never affects refs or URLs.

Cross-references in spec fields (e.g. `policy.spec.provider`, `route.spec.models[]`) currently store the **slug**, not the id — so renames-via-displayName are trivially safe. Slug edit isn't yet implemented; when added it will need a referencing-rewriter.

`pkg/ids` (UUIDv7) and `pkg/slug` (slugify + collision suffix) are the pure helpers.

### Admin CRUD surface

Six generic kinds: `providers`, `policies`, `secrets`, `models`, `routes`, `ratelimits`, plus `keys` (RelayKey). Per-kind routes:

```
GET    /control/{kind}                 list
GET    /control/{kind}/{ref}           read by slug or id (UUID form prefers id)
POST   /control/{kind}                 create — server stamps id+slug from displayName
PUT    /control/{kind}/by-id/{id}      update (id-routed; body may change slug)
DELETE /control/{kind}/by-id/{id}      delete (id-routed)
```

Plus `/control/attachments` (polymorphic rate-limit → resource links).

**Slug-routed exceptions** (not yet moved to id-routing):
- `/control/secrets/{name}` — bespoke handler in `cmd/relay/openapi.go`
- `/control/keys/{name}/revoke`, `/restore` — relay-key revoke/restore
- `/control/passthrough` — singleton; identity not relevant

Other notes:
- Handlers live in `cmd/relay/` (`admin_handlers.go`, `admin_secret_attachment_handlers.go`)
- Generic CRUD factory lives in `pkg/admin/crud` (Go package path; the HTTP surface is `/control/*`)
- Pre-write validation (snapshot + proposed patch) lives in `pkg/configstore` (`ValidateWithPatch`)
- Secrets support two modes: `valueFrom: {kind: env, env: VAR_NAME}` (env-ref, no creds in PG) and `valueFrom: {kind: stored, value: sk-...}` (AES-GCM-256 encrypted with `RELAY_MASTER_KEY`, ciphertext in PG)
- Every write auto-reloads the snapshot; no manual `/control/reload` needed for CRUD operations

### Hot-path rules (non-negotiable)
- No Postgres calls on the request path
- Default: full body parse to a typed shape-specific struct (`RELAY_RICH_PARSING=on`). `off` reverts to the legacy minimal-parse path (model/stream/user/raw only). Always: `messages` content not deep-parsed; raw body retained for byte-equivalent upstream forward. Pure shape types/parsers/transformers live in `pkg/api/<shape>` (no Relay imports — vendorable); HTTP handlers and Relay-specific glue stay in `internal/api/<shape>`. `pkg/transport` is shape-agnostic. Token counts come from the provider response.
- **One** Redis Lua call per request (auth + rate limit + quota + key-pool snapshot in one atomic script). Not three.
- No mid-stream failover. Failover only pre-first-byte.
- No middleware/plugin chain à la LiteLLM
- All emits (usage → ClickHouse, span → OTel, payload → S3) are async via bounded channels with drop-on-full + drop counter

### `pkg/kv` consumer conventions

1. Hash-tag every kv key with `{tag}:` where tag is the consumer's atomicity boundary.
2. All keys touched in one Lua script (`RunScript`) or `WithLock` must share the same `{tag}` substring.
3. Centralize key construction in a `keys.go` per consumer — no inline key strings at call sites.
4. Each consumer declares its own narrow interface listing only the kv methods it uses; don't import a fat `kv.Store` everywhere.
5. Every key must have a TTL unless justified in a code comment. Persistent data goes in Postgres.
6. Tests must pass against both `kv.Mem` and `kv.Redis` backends.
7. Document expected kv ops per request in the consumer's package doc comment.

Full guide: `docs/kv.md`.

### Storage layer conventions

`internal/storage` owns the entire Postgres surface. Domain packages (`internal/catalog`, future `internal/audit`, `internal/batch`) call typed storage methods and never see SQL or pgx.

1. No `pgx`, `pgxpool`, or `pgconn` types in any signature outside `internal/storage`.
2. No SQL strings (`SELECT`/`INSERT`/`UPDATE`/`DELETE`) in `.go` files outside `internal/storage`.
3. No sqlc-generated types in exported signatures — convert to domain types at the storage boundary.
4. Transactions are storage-managed: domain code uses `storage.WithTx(ctx, func(tx *Storage) error {...})`, never holds a `*pgx.Tx`.
5. Storage returns domain errors (`catalog.ErrNotFound`, `catalog.ErrConflict`), never `pgx.ErrNoRows` or `*pgconn.PgError`.
6. Group storage by domain area, not by entity: `s.Catalog.UpsertProvider`, `s.Audit.Append` — not flat methods or one-repo-per-entity.
7. Encryption policy lives in domain (`internal/catalog`), primitives in `pkg/crypto`. Storage handles already-encrypted bytes; the master key never enters `internal/storage`.

Full guide: `docs/storage.md`.

### Cluster mode

`RELAY_CLUSTER_MODE=on` enables multi-pod coordination — currently a PG NOTIFY/LISTEN catalog watcher; future: leader election, Redis Cluster client. Default `off` for single-pod. Producers (NOTIFY emit on catalog write) are unconditional; only consumers/listeners are gated by the flag.

Full guide: `docs/cluster.md`.

### Streaming
- Tee model: bytes pass through, parser goroutine extracts usage from a copy
- Cross-format reserialize is a slower path; same-format passthrough is the 95% case

### Key pooling
- Failover + load-balanced within a single tenant
- Weighted random by remaining quota; per-key Redis circuit breakers
- No cross-tenant pooling

### Batch
- Relay primitive, not a provider feature — works for any upstream
- Use provider batch APIs when available (50% discount passthrough); simulate otherwise via worker pool
- Customer interface: submit → poll OR webhook → fetch from S3

### Observability
- OTel for traces (one span per request, rich attributes including decision trace)
- Prometheus for pod metrics
- ClickHouse internal for analytics
- Structured JSON logs to stdout

## Performance contract

Two regimes — be precise about which one you're quoting:

**Live distributed deployment** (real Redis Lua RTT, real PG, two-pod fleet, nginx LB):
- p50 overhead: < 2 ms
- p99 overhead: 10 ms (internal SLO), 15 ms (public claim)

**In-process bench** (`bench/bench_test.go`, in-memory `kv.Mem`, no network):
- p50 overhead: < 100 µs
- p99 overhead: < 500 µs

The 18× gap between the two is unavoidable I/O — Redis Reserve Lua RTT, nginx hop, container network, retry/circuit logic against real Redis. Use bench numbers as a regression gate (architecture's lower bound); use live numbers for SLO conversations.

- RPS per pod: 5–10k (unchanged)
- Tier-3 totals via horizontal scale, not per-pod heroics
- Load-test on every PR; fail builds on p99 regression
- Post-flight (rate-limit Commit, keypool RecordSuccess) runs in a detached goroutine — does NOT block the response. Tracked via `relay_pipeline_post_flight_duration_seconds`.

## Code style and conventions

- Go 1.25+
- Module: `github.com/wyolet/relay`
- Hot path code must be allocation-conscious. Use `sync.Pool` for buffers; avoid string conversions; reuse header maps.
- `GOMEMLIMIT` and tuned `GOGC` are part of the deployment story.
- Async work uses bounded channels with explicit drop-on-full and a Prometheus drop counter — never unbounded queues, never block-on-send on the hot path.
- gRPC is reserved for **internal** control-plane ↔ data-plane traffic only. The customer-facing edge is HTTP/JSON.

## When in doubt

- Don't over-engineer. The user actively flags overengineering tendencies — push back when a feature isn't earning its complexity.
- Boring choices on the edges, smart choices in the middle.
- "Three similar lines is better than a premature abstraction."
- Read the Obsidian docs (`~/Documents/Obsidian Vault/Projects/Relay/`) before proposing architecture changes.
