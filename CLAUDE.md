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

### Admin CRUD surface

`/admin/{kind}` (list/create) and `/admin/{kind}/:name` (get/update/delete) for six kinds: `providers`, `pools`, `secrets`, `models`, `routes`, `ratelimits`. Plus `/admin/attachments` (polymorphic rate-limit → resource links).

- Handlers live in `cmd/relay/` (`admin_handlers.go`, `admin_secret_attachment_handlers.go`)
- Generic CRUD factory lives in `pkg/admin/crud`
- Pre-write validation (snapshot + proposed patch) lives in `pkg/configstore` (`ValidateWithPatch`)
- Secrets support two modes: `valueFrom: {kind: env, env: VAR_NAME}` (env-ref, no creds in PG) and `valueFrom: {kind: stored, value: sk-...}` (AES-GCM-256 encrypted with `RELAY_MASTER_KEY`, ciphertext in PG)
- Every write auto-reloads the snapshot; no manual `/admin/reload` needed for CRUD operations

### Hot-path rules (non-negotiable)
- No Postgres calls on the request path
- Default: full body parse to a typed shape-specific struct (`RELAY_RICH_PARSING=on`). `off` reverts to the legacy minimal-parse path (model/stream/user/raw only). Always: `messages` content not deep-parsed; raw body retained for byte-equivalent upstream forward. Shape-specific parsed types live in `pkg/api/<shape>` — `pkg/transport` is shape-agnostic. Token counts come from the provider response.
- **One** Redis Lua call per request (auth + rate limit + quota + key-pool snapshot in one atomic script). Not three.
- No mid-stream failover. Failover only pre-first-byte.
- No middleware/plugin chain à la LiteLLM
- All emits (usage → ClickHouse, span → OTel, payload → S3) are async via bounded channels with drop-on-full + drop counter

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

- p50 overhead: <1ms (internal)
- p99 overhead: 5ms (internal SLO), 10ms (public claim)
- RPS per pod: 5–10k
- Tier-3 totals via horizontal scale, not per-pod heroics
- Load-test on every PR; fail builds on p99 regression

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
