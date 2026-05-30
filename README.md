# Wyolet Relay

High-throughput LLM router in Go. Self-hostable, k8s-native, BYO
provider keys. A faster, infra-grade alternative to OpenRouter and
LiteLLM for teams running millions of LLM requests per day.

## What it does

- **Unified API** in front of OpenAI- and Anthropic-shape upstreams
  (OpenAI, Anthropic, Bedrock, Vertex, Azure OpenAI, Ollama, Together,
  Groq, Fireworks — anything that speaks one of those wire protocols).
- **API key pooling** — failover and load-balanced multi-key
  concurrency to multiply effective rate limits. Per-key circuit
  breakers with auth/quota/rate-limit/server-error classification.
- **Pluggable secret backends** — BYO keys resolved from env, an
  AES-GCM Postgres store, or fetch-only external stores (AWS Secrets
  Manager, Azure Key Vault, GCP Secret Manager, Vaultwarden/Bitwarden,
  1Password). External secrets are held in memory only, never persisted.
  On an upstream auth failure the key is re-resolved out-of-band: the
  request fails over to another key/host immediately and the rotated
  key heals in the background (or, as a last resort, parks briefly and
  retries with the fresh value).
- **Adapter dispatch on (Model, Host)** — one Host (e.g. AWS Bedrock)
  can serve models on different wire protocols; binding-level
  `adapter: openai | anthropic` picks correctly per request.
- **Generated typed OpenAPI** for both planes — frontend / CLI clients
  get full type information for free.
- **Postgres-backed catalog with NOTIFY/LISTEN snapshot fan-out** —
  admin writes propagate to every pod within ~1 second without a
  hot-path PG round-trip.
- **Streaming bytes pass through** — the orchestration layer is shape-
  agnostic; SSE and JSON responses stream byte-for-byte from upstream.

## Status

| Area | State |
|---|---|
| Catalog (8 entities, snapshot, NOTIFY listener) | Shipped |
| Admin API (auth, CRUD, sessions) | Shipped |
| Data-plane `/v1/*` (chat completions, messages, responses, embeddings, models) | Shipped |
| Adapter dispatch per HostBinding | Shipped |
| **Canonical protocol `pkg/relay/v1/`** (typed item array, six-event streams) | **Shipped (PR #185)** |
| **Cross-vendor translation via canonical** (Anthropic ↔ canonical ↔ CC verified end-to-end) | **Shipped (PRs #186–187)** |
| **Generic `app/adapter/` framework** (no per-vendor packages in `app/`) | **Shipped (PR #189)** |
| Mock-based smoke (`make smoke-mock`) | **Shipped** — fixture replay through real Caddy edge |
| **Secret backends** (env, stored, AWS/Azure/GCP/Vaultwarden/1Password) | **Shipped** — fetch-only, in-memory; 1Password behind `cgo` build tag |
| **Secret rotation healing** (`pipeline.KeyAgent`) | **Shipped** — out-of-band re-resolve on auth failure |
| **Catalog `/catalog/resolve` + `/catalog/graph`** | **Shipped** — snapshot-backed ref expansion + minimal picker graph |
| Observability emit (OTel + ClickHouse) | Scaffolding present; **rewiring is roadmap A2** |
| Multi-tenancy (Org/Workspace/Project) | **roadmap B2** |
| Postgres-backed users + signup | **roadmap B1**; today identity is YAML |
| Real authz (Casbin) | **roadmap B3**; today is "any authenticated caller can do anything" |
| Batch API | **roadmap D** |

See [`docs/roadmap.md`](docs/roadmap.md) for the full picture and
[`docs/canonical-protocol.md`](docs/canonical-protocol.md) for the
relay-canonical protocol design.

## Quickstart

### Docker (standalone — one command)

Requires Docker. Bundles its own Postgres, ClickHouse, Valkey, and Jaeger;
builds the relay image from source (no published image yet).

```bash
cp .env.example .env
# REQUIRED: set a master key in .env — generate one with:
#   openssl rand -base64 32
docker compose up --build
```

Data plane: `http://localhost:5100` · control plane: `http://localhost:5103`.

### Run from source (dev)

Requires Go 1.25+, Docker, and `make`. Ephemeral Postgres via compose.

```bash
# Bring the test PG up.
docker compose -f deploy/compose/docker-compose.test.yml up -d --wait

# Run the binary.
RELAY_PG_DSN='postgres://relay:relay@127.0.0.1:5499/relay_test?sslmode=disable' \
  RELAY_AUTO_SEED_IF_EMPTY=1 \
  RELAY_CONFIG_DIR=./config \
  RELAY_PORT=5199 \
  RELAY_CONTROL_PORT=5198 \
  RELAY_STATE_BACKEND=memory \
  RELAY_ADMIN_TOKEN=devtoken \
  RELAY_ADMIN_PASSWORD=devpass-min-8-chars \
  RELAY_COOKIE_SECURE=false \
  go run ./cmd/relay
```

On first boot the relay auto-seeds 3 providers, 3 hosts, 132 models,
and 110 pricings from `./config/`.

Verify:

```bash
# Inference healthcheck (public)
curl localhost:5199/healthz

# Control version (public)
curl localhost:5198/version

# Admin CRUD with break-glass bearer
curl -H 'Authorization: Bearer devtoken' localhost:5198/providers
```

Tear down:

```bash
docker compose -f deploy/compose/docker-compose.test.yml down -v
```

## Architecture

Two listeners on separate ports, sharing one in-memory catalog
snapshot:

```
                ┌───────────────────────────┐
                │   Postgres (truth)        │
                │   + Redis (hot state)     │
                └───────────────────────────┘
                            ▲
                            │ NOTIFY catalog_events
                            │
                   ┌────────┴────────┐
                   │  app/catalog    │
                   │  Snapshot       │  COW reconciler, dependency-ordered
                   │  + Listener     │  apply, debounced ~1s
                   └────────┬────────┘
                            │
        ┌───────────────────┴───────────────────┐
        │                                       │
┌───────▼────────┐                     ┌────────▼────────┐
│ Inference plane│                     │ Control plane   │
│ RELAY_PORT     │                     │ RELAY_CONTROL_  │
│                │                     │ PORT            │
│ /v1/*          │                     │ /auth/*         │
│ /healthz       │                     │ /{kind}/...     │
│                │                     │ /master-key/... │
│ relay-key auth │                     │ session cookie  │
│                │                     │  + admin token  │
└────────┬───────┘                     └─────────────────┘
         │
         ▼
   app/pipeline ────► app/api/<adapter> ────► upstream
   (orchestration)    (wire format)
```

**Component responsibilities** (full breakdown in [`CLAUDE.md`](CLAUDE.md)):

- **`app/catalog`** — immutable snapshot, COW reconciler, NOTIFY
  listener. Reads are O(1) by id or by name. Writes go through
  `app/X.Store` → PG → NOTIFY → listener → reconciler → atomic snapshot
  swap.
- **`app/pipeline`** — pure orchestration. Reserve rate-limit budget →
  pick a host key → call upstream via `Adapter.Call` → stream response
  → detached post-flight (commit + RecordSuccess + OnSuccess
  callback). Knows nothing about catalog snapshots, JSON shapes, or
  HTTP routing.
- **`app/api/{openai,anthropic}`** — adapter implementations. Build
  the upstream HTTP request, classify response status for retry,
  extract usage tokens. Pure shape parsers live separately under
  `pkg/api/<shape>` (vendorable).
- **`app/httpapi/{inference,control}`** — huma + chi HTTP layer. Each
  plane has its own generated OpenAPI spec.

## API surface

### Inference plane (data plane)

```
POST   /v1/generate                  relay canonical shape (provider-neutral)
POST   /openai/v1/chat/completions   OpenAI Chat Completions shape
POST   /openai/v1/responses          OpenAI Responses shape
POST   /openai/v1/embeddings         OpenAI Embeddings (byte-pass)
POST   /anthropic/v1/messages        Anthropic Messages shape
GET    /v1/models                    list models accessible to the relay key
GET    /healthz                      liveness + PG ping (public)
GET    /openapi.json                 generated typed spec
```

Auth: `Authorization: Bearer <relay-key>`. Relay keys are managed via
the control plane.

Namespace convention: each vendor shape is served under its own prefix
(`/openai/...`, `/anthropic/...`); the bare `/v1` namespace belongs to
relay's own canonical shape. `POST /v1/generate` accepts a canonical
`v1.Request` and returns canonical — the provider-neutral surface that
carries cross-cutting features like `cache_config` to any upstream
regardless of the model's `HostBinding` adapter.

### Control plane (admin)

Uniform CRUD across eight kinds:

```
GET    /{plural}                 list
GET    /{plural}/{ref}           read by slug or id
POST   /{plural}                 create (server stamps id + slug)
PUT    /{plural}/by-id/{id}      update
DELETE /{plural}/by-id/{id}      delete
```

Plurals: `providers`, `hosts`, `models`, `host-keys`, `rate-limits`,
`policies`, `pricings`, `relay-keys`.

Plus:

```
POST   /auth/login               username + password → session cookie
POST   /auth/logout              destroy session
GET    /auth/whoami              return the authenticated caller
POST   /master-key/generate      mint a candidate RELAY_MASTER_KEY
POST   /reload                   force full snapshot rebuild from PG
GET    /version                  build version
GET    /openapi.json             generated typed spec
```

Auth: either a valid `relay_session` cookie (set by `/auth/login`) OR
`Authorization: Bearer ${RELAY_ADMIN_TOKEN}` (break-glass).

## Configuration

| Env var | Default | Description |
|---|---|---|
| `RELAY_PG_DSN` | _(required)_ | Postgres connection string. |
| `RELAY_PORT` | `8080` | Inference plane listener port. |
| `RELAY_CONTROL_PORT` | _(empty = off)_ | Control plane listener port. |
| `RELAY_STATE_BACKEND` | `memory` | `memory` or `redis`. |
| `RELAY_REDIS_ADDR` | _(empty)_ | Required when `RELAY_STATE_BACKEND=redis`. |
| `RELAY_AUTO_SEED_IF_EMPTY` | _(empty)_ | When `1` and PG is empty, seed from `RELAY_CONFIG_DIR`. |
| `RELAY_CONFIG_DIR` | `config` | YAML config tree for auto-seed + identity. |
| `RELAY_ADMIN_TOKEN` | _(empty)_ | Break-glass control-plane bearer. Empty disables. |
| `RELAY_MASTER_KEY` | _(empty)_ | 32-byte base64 master key for stored-mode HostKeys. Generate via `POST /master-key/generate`. |
| `RELAY_COOKIE_SECURE` | _(unset = true)_ | Set to `false` for HTTP-only local dev. |
| `RELAY_SHUTDOWN_DEADLINE_S` | `15` | Graceful shutdown budget. |
| _(rich parsing)_ | `on` | Moved to the `parsing` settings section (`PUT /settings/parsing {"richParsing": false}`). Hot-reloaded; no env var, no restart. |

## Auth model

| Caller | Endpoint | Mechanism |
|---|---|---|
| Browser → control | `/auth/*`, `/{kind}/*`, `/master-key/*`, `/reload` | `relay_session` cookie (scs + kv-backed) |
| Operator / CI → control | same | `Authorization: Bearer ${RELAY_ADMIN_TOKEN}` |
| Customer code → inference | `/v1/*` | `Authorization: Bearer <relay-key>` |

Passwords are bcrypt-aware: hashes starting with `$2a$ / $2b$ / $2y$`
go through `bcrypt.CompareHashAndPassword`; plain-text passwords work
for legacy YAML but log a deprecation warning.

Future-proof seams (already wired):

- `app/actor.Actor` in `context.Context` — handlers read it via
  `actor.From(ctx)`, never the cookie or token directly.
- `app/authz.Authorizer` interface — every CRUD mutation routes
  through `Authorize(ctx, action, resource)`. v1 implementation
  grants any authenticated caller; swap to Casbin / OpenFGA later
  without touching handlers.

JWT for programmatic API access is a planned third caller type
([`docs/roadmap.md`](docs/roadmap.md) B4).

## Performance

| | p50 | p99 | RPS per pod |
|---|---|---|---|
| Live (real Redis + PG + LB) | < 2 ms overhead | 10 ms (SLO), 15 ms (public) | 5–10k |
| In-process bench (`kv.Mem`) | < 100 µs | < 500 µs | — |

The 18× gap is unavoidable I/O — Redis Lua RTT, LB hop, container
network. Bench numbers are the architectural lower bound (regression
gate); live numbers are what you can promise customers.

Hot-path rules ([`CLAUDE.md`](CLAUDE.md)):

- Zero PG calls on the request path.
- One Redis Lua call (rate-limit reserve) is the goal; not three round-trips.
- Post-flight (`Limiter.Commit`, `Selector.RecordSuccess`,
  `OnSuccess`) runs in a detached goroutine — never blocks the
  response.
- All emits (usage, spans, payload capture) are async via bounded
  channels with drop-on-full.

**Note**: the in-process bench harness was retired with the legacy
pipeline; a new harness against `app/pipeline.Pipeline.Run` is
[`docs/roadmap.md`](docs/roadmap.md) A3.

## Repository layout

Full breakdown in [`CLAUDE.md`](CLAUDE.md). Short version:

```
app/                  application: catalog, pipeline, http handlers, 8 entities
pkg/                  shared, vendorable: shapes, ratelimit, kv, ids, slug, ...
internal/             composition root: config, identity, storage, usage
cmd/relay/            binary entrypoint
cmd/litellm-import/   regenerate config YAML from LiteLLM JSON
config/               operator-authored YAML catalog
migrations/postgres/  versioned SQL up + down
deploy/compose/       dev pg, test pg, smoke stack
docs/                 design notes + roadmap + runbook
```

## Development

```bash
# Unit tests
go test ./...

# Integration suite (spins up ephemeral pg, tears down)
make test-integration

# Regenerate config YAMLs from LiteLLM
go run ./cmd/litellm-import --out config

# Rebuild the dev stack
make dev
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the build/test workflow, the
load-bearing codebase rules, and PR conventions. `make lint-rules`
enforces the canonical-protocol import boundaries (rules 1/2/4/10).

## Roadmap

The roadmap is split by product phase — [`docs/roadmap.md`](docs/roadmap.md)
is the index:

- [`roadmap-oss.md`](docs/roadmap-oss.md) — the open-source core: batch
  orchestration, webhooks, new adapters, observability, and launch readiness.
- [`roadmap-enterprise.md`](docs/roadmap-enterprise.md) — on-prem: authN/authz,
  SSO, audit, HA, air-gap, security-review readiness.
- [`roadmap-saas.md`](docs/roadmap-saas.md) — hosted multi-tenant: billing,
  quotas, tenant isolation, compliance.
- [`roadmap-v2.md`](docs/roadmap-v2.md) — beyond v1: the Tool Gateway + icebox.

## License

[AGPL-3.0](LICENSE). Network use triggers the copyleft — run a modified
relay as a service and you must publish your changes. Commercial /
enterprise licensing is a separate arrangement.

