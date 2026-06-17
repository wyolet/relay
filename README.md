# Wyolet Relay

**A high-throughput LLM router in Go.** Put one endpoint in front of every
provider, pool your own API keys for failover and higher effective rate
limits, and self-host the whole thing. Built for teams running millions of
LLM requests a day.

```bash
docker run -p 8080:8080 -p 8081:8081 wyolet/relay:standalone
```

A full relay with Postgres bundled and the catalog pre-seeded — admin UI on
**:8081**, OpenAI/Anthropic-compatible API on **:8080**. The master key, admin
password, and admin token are auto-generated on first boot and printed to the
logs (and persisted on the data volume). See
[Quickstart](#quickstart--first-request-in-2-minutes) for your first request.

## Why Relay

- **One API, every upstream.** OpenAI- and Anthropic-shape endpoints
  (OpenAI, Anthropic, Bedrock, Vertex, Azure OpenAI, Ollama, Together, Groq,
  Fireworks — anything speaking either wire protocol). Drop-in for your
  existing OpenAI/Anthropic SDK code.
- **Key pooling that multiplies your rate limits.** Load-balanced, failover
  across many keys, with per-key circuit breakers (auth / quota / rate-limit
  / server-error aware). On an upstream auth failure a key is re-resolved
  out-of-band — the request fails over immediately and the rotated key heals
  in the background.
- **BYO keys, your secret store.** Resolve keys from env, an AES-GCM
  Postgres store, or fetch-only external backends (AWS / Azure / GCP Secret
  Manager, Vaultwarden/Bitwarden, 1Password). External secrets stay in
  memory, never persisted.
- **Infra-grade performance.** < 2 ms p50 added latency, 5–10k RPS/pod,
  k8s-native. Postgres-backed catalog fans out to every pod via NOTIFY/LISTEN
  in ~1s with zero PG round-trips on the hot path; responses stream
  byte-for-byte from upstream.
- **Self-hostable, AGPL-3.0.** Your keys, your data, your infra. Generated
  typed OpenAPI for both planes, so clients get full type information free.

## Quickstart — first request in ~2 minutes

**1. Start the relay** (the `docker run` above). Also on GitHub Container
Registry as `ghcr.io/wyolet/relay:standalone`.

**2. Run the setup wizard.** Open `http://localhost:8081`, log in with the
admin credentials you set (`RELAY_ADMIN_TOKEN` / `RELAY_ADMIN_PASSWORD`). The
wizard walks you through adding a provider key and minting a relay key — copy
the relay-key plaintext when it's shown (it's shown exactly once).

**3. Call it** like the OpenAI API:

```bash
curl http://localhost:8080/openai/v1/chat/completions \
  -H "Authorization: Bearer <your-relay-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

The bundled image is single-node with embedded Postgres — perfect for a
try-out, not for production. For that, run the lean image
(`wyolet/relay:latest`) against a managed Postgres; see
[other ways to run](#other-ways-to-run) below.

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

See [`design/roadmap.md`](design/roadmap.md) for the full picture and
[`design/canonical-protocol.md`](design/canonical-protocol.md) for the
relay-canonical protocol design.

## Other ways to run

Official images (`:latest` lean, `:standalone` all-in-one, and `:<version>`
tags) are published to Docker Hub (`wyolet/relay`) and GHCR
(`ghcr.io/wyolet/relay`) by the maintainers; this repository does not build or
publish them in CI. To build your own from source, use the `Dockerfile` /
`docker-bake.hcl`, or the dev stack under `deploy/compose/`.

### Docker Compose (relay + Postgres + Valkey)

The repo root ships a self-contained stack — the lean relay image wired to its
own Postgres and Valkey, with volumes and environment preconfigured:

```bash
docker compose up -d
```

Inference API → `http://localhost:8080` · **admin UI** + control API →
`http://localhost:8081`. The image bakes in the catalog and seeds it into
Postgres on first boot. Log in as `admin` / `RELAY_ADMIN_PASSWORD` (default
`change-me-please`), then add a host key + mint a relay key.

Before a real deployment, set a stable `RELAY_MASTER_KEY` and change
`RELAY_ADMIN_PASSWORD` — put them in a `.env` next to the compose file, or edit
the defaults inline.

For the multi-pod dev stack (nginx load balancer + two replicas, built from
source), see `deploy/compose/` and `make dev`.

### Run from source (dev)

Requires Go 1.25+, Docker, and `make`. Ephemeral Postgres via compose.
Seeding from source needs a local clone of the catalog data — the
[`wyolet/relay-catalog`](https://github.com/wyolet/relay-catalog) repo's
`data/` tree (cloned alongside this repo below):

```bash
# Catalog data, cloned next to this repo.
git clone https://github.com/wyolet/relay-catalog ../relay-catalog

# Bring the test PG up.
docker compose -f deploy/compose/docker-compose.test.yml up -d --wait

# Run the binary.
RELAY_PG_DSN='postgres://relay:relay@127.0.0.1:5499/relay_test?sslmode=disable' \
  RELAY_AUTO_SEED_IF_EMPTY=1 \
  RELAY_CATALOG_DIR=../relay-catalog/data \
  RELAY_CONFIG_DIR=./config \
  RELAY_PORT=5199 \
  RELAY_CONTROL_PORT=5198 \
  RELAY_STATE_BACKEND=memory \
  RELAY_ADMIN_TOKEN=devtoken \
  RELAY_ADMIN_PASSWORD=devpass-min-8-chars \
  RELAY_COOKIE_SECURE=false \
  go run ./cmd/relay
```

On first boot the relay auto-seeds the catalog from `RELAY_CATALOG_DIR` into the
empty Postgres (providers, hosts, models, bindings, pricing, policies). Omit
`RELAY_CATALOG_DIR` to start with an empty catalog and populate it via the
control API. (The Docker image above bakes the catalog in, so it needs no clone.)

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
| `RELAY_PG_LOG` | `quiet` | _(standalone image only)_ Bundled Postgres logs to a file on the data volume, quieted to warnings. Set `stdout` to stream raw PG logs to the container. |
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
([`design/roadmap.md`](design/roadmap.md) B4).

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
[`design/roadmap.md`](design/roadmap.md) A3.

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
design/                 design notes + roadmap + runbook
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

[`design/roadmap.md`](design/roadmap.md) is the index and shared changelog;
[`roadmap-oss.md`](design/roadmap-oss.md) tracks the open-source core: batch
orchestration, webhooks, new adapters, observability, and launch readiness.

## License

[AGPL-3.0](LICENSE). Network use triggers the copyleft — run a modified
relay as a service and you must publish your changes. Commercial /
enterprise licensing is a separate arrangement.

