# CLAUDE.md

Guidance for Claude Code working in this repo.

## Dev workflow

Wyolet dev workflow — read these before any non-trivial change. File
layout, ports, infra split, build/deploy pipeline live there, not
duplicated here:

- `~/Documents/Obsidian Vault/Dev Workflow/Development.md` — Mac +
  dev-stack split, ports rule, Caddy, centralised Postgres
- `~/Documents/Obsidian Vault/Dev Workflow/ProjectSetup.md` — required
  files (Makefile, compose, Dockerfile, bake, .env), stack rules
- `~/Documents/Obsidian Vault/Dev Workflow/BuildDeploy.md` — Harbor +
  ghcr.io, `kube` buildx context, ArgoCD reconcile
- `~/Documents/Obsidian Vault/Dev Workflow/PORTS.md` — LAN port
  allocations

## What this is

Wyolet Relay — a high-throughput LLM router in Go. Self-hostable,
k8s-native, BYO provider keys. Competes with OpenRouter and LiteLLM on
the infrastructure axis (performance, key pooling, batch orchestration,
observability).

Roadmap and stage breakdown: `docs/roadmap.md`. Full design context
lives in the user's Obsidian vault at
`~/Documents/Obsidian Vault/Projects/Relay/` — read before non-trivial
architectural suggestions.

## Wedge — what we are NOT

- Not a marketplace / reseller of provider tokens (deferred indefinitely)
- Not Python — performance is a wedge, runtime cost matters
- Not a drop-in clone of LiteLLM with a Go badge — feature parity isn't
  the goal, infra-grade throughput is
- Not adaptive / quality-aware routing in v1 — bad auto-routing loses
  customers
- Not a custom wire protocol — OpenAI/Anthropic shapes are accepted as
  passthrough

## Repository layout

The codebase is organised by responsibility, not by entity:

```
app/                       — the application: domain + composition + handlers
  catalog/                 — composition layer: immutable Snapshot, COW
                             reconciler, NOTIFY listener, Bootstrap()
  {provider,host,model,
   hostkey,ratelimit,
   policy,relaykey,pricing}/  — 8 entity packages
                             — each: domain types, Validate(), Store{List,
                                     Get,Upsert,Delete}
  adapter/                 — wire-protocol Kind enum (openai, anthropic)
  pipeline/                — pure orchestration: reserve → pick key →
                             Adapter.Call → stream → post-flight emit
  routing/                 — snapshot lookup → Plan{Model, Policy,
                             HostBinding, Host, Keys, Rules}
  api/openai/              — pipeline.Adapter impl (OpenAI wire shape)
  api/anthropic/           — pipeline.Adapter impl (Anthropic wire shape)
  keypool/                 — key selection + per-key circuit breaker
  ratelimit/               — Resolve(policy, rl) → []pkgratelimit.Rule
  httpapi/                 — the HTTP layer
    inference/             — data plane: /v1/* + /healthz
    control/               — admin plane: /auth/* + CRUD + /version + ...
  manifest/                — YAML DTOs + translate ↔ domain
  seed/                    — YAML → Postgres orchestration
  session/                 — scs wrapper, kv-backed
  authz/                   — Authorizer interface (v1: AlwaysAllow)
  actor/                   — Actor in context (user/admin-token)

pkg/                       — shared, vendorable, no Relay-specific imports
  api/{openai,anthropic}/  — pure shape: parse, types, token extraction
  ratelimit/               — Limiter (kv-backed Lua) + Rule + Reservation
  kv/                      — Store interface + Mem + Redis backends
  eventlog/                — file + ClickHouse usage event sinks
  crypto/                  — AES-GCM helpers (master-key)
  httpmw/, httpheader/     — net/http middleware + header helpers
  ids/, slug/              — UUIDv7, slug minting + collision suffixes
  metrics/                 — Prometheus registry + counters
  reqid/                   — request-id middleware (+ OTel span)
  transport/               — message channel (currently only used by
                             legacy paths; candidate for deletion)
  usage/                   — Tokens type alias

internal/                  — composition root / boundary
  config/                  — RELAY_* env parsing
  identity/                — YAML-loaded users (Verify with bcrypt-or-plain)
  storage/                 — pgx pool + migrations; exposes Pool() only
  usage/                   — observability scaffolding (NOT wired post-cutover;
                             see docs/roadmap.md A2)

cmd/relay/                 — the binary entrypoint
cmd/litellm-import/        — fetches LiteLLM JSON → manifest YAMLs

config/                    — relay-local YAML (NOT the public catalog)
  ratelimits/system.yaml   — relay-internal admission/DoS rules
  users/                   — deployment-specific admin credentials

migrations/postgres/       — versioned SQL up + down

deploy/compose/            — dev pg, test pg, smoke stack
docs/                      — design + runbook + roadmap
```

## Public catalog lives in a separate repo

The provider/host/model/pricing/policy YAMLs are NOT in this repo. They
live in `wyolet/relay-catalog` and are consumed by the seed loader at
boot (released tarball, pinned by tag via `RELAY_CATALOG_VERSION`, with
a `go:embed`'d default baked into the relay binary for airgapped /
first-boot scenarios).

What this repo owns:

- `config/ratelimits/system.yaml` — relay-internal admission rules.
- `config/users/` — deployment-specific admin credentials.
- The seed *loader* code (`app/seed`), the manifest DTOs (`app/manifest`),
  the schema validator (`*.Validate()`), and the `cmd/catalog-validate`
  binary the catalog repo's CI invokes.

What `wyolet/relay-catalog` owns (under its `data/` root):

```
data/
  providers/<provider>/provider.yaml
                       models/<model>.yaml
  hosts/<host>/host.yaml
               pricing/<model>.yaml
               policies/<policy>.yaml          (Policy + optional RateLimit
                                                in one file, --- separated)
```

Ownership drives placement: providers own their models, hosts own their
pricing + tier policies + the tier-policies' RateLimits. Filenames omit
the parent prefix (e.g. `pricing/gpt-4o.yaml` not `openai-gpt-4o.yaml`),
but `metadata.name` still carries the full slug for cross-refs.

- Each release will publish a tarball + sidecar `index.json` + sha256.
- Schema is versioned by the `apiVersion` field; bumping the relay's
  schema means cutting a matching tag in the catalog repo.

When you need a sample YAML to reason about, check
`wyolet/relay-catalog`'s `main` branch (or pull a tagged tarball). Do
NOT regenerate them in this repo.

**Local consumption (today, until the tarball fetcher lands):**

- `RELAY_CONFIG_DIR` (default `config`) — relay-internal yamls only.
- `RELAY_CATALOG_DIR` — local clone of `wyolet/relay-catalog`'s `data/`
  tree. When set + `RELAY_AUTO_SEED_IF_EMPTY=1` + PG is empty, Bootstrap
  walks it recursively and seeds. Unset disables auto-seed.
- The recursive `manifest.LoadDir` is layout-agnostic — dispatches on
  each YAML's `kind` field, so the nested catalog tree just works.

## Locked architectural decisions

The "spine" is the **Domain Model + Storage Layer**, not the request
pipeline:

```
Domain & Storage:  Postgres (config) + Redis (hot/counters) +
                   ClickHouse (events) + S3 (opt-in payloads)
Subsystems:        Realtime data plane | Batch workers | Control plane
```

- **Control plane** owns Postgres truth, publishes via PG NOTIFY
- **Data plane** is stateless, reads from the in-memory snapshot only
- **Batch subsystem** reuses the realtime path's Adapter and keypool
  Selector — does not fork

### Domain model (current state)

```
Provider (vendor display)  ──┐
Host     (serving endpoint) ─┤
HostKey  (upstream credential, owner = Host)
RateLimit (rule set)         │
                              │
Policy   (hub) ─── ModelIDs ──┴── Model ─── HostBinding[]
            ├──── HostKeyIDs                     ├── HostID
            └──── RateLimitID                    ├── UpstreamName
                                                 ├── Adapter (openai|anthropic)
                                                 └── Enabled
Pricing  (owner=Host, applied to N Models, tier-aware via AboveTokens)

RelayKey (inbound customer API key → PolicyID)
```

**Route entity is deferred** — Policy + RelayKey cover the v1 case
("customer hits /v1/chat/completions with bearer key X; key has Policy
Y; Policy Y allows Model M served via Host H with Adapter A"). When
multi-tenancy lands the Route + Org/Project hierarchy comes back per
`docs/roadmap.md` track B.

### Identity model (every catalog resource)

Three-field identity, each with a distinct role:

- `metadata.id` — UUIDv7, **immutable**, server-stamped on create. The
  PG primary key. Used in all id-routed admin URLs.
- `metadata.name` — DNS-1123 slug, stable, mutable. Auto-derived from
  `displayName` on create with a collision suffix. Used in human-
  readable URLs and YAML refs.
- `metadata.displayName` — free text. Edits are free; nothing
  references it.

Cross-references in spec fields (e.g. `policy.spec.modelIds`,
`pricing.spec.targetModelIds`) store **ids**, not slugs (migration
0009). YAML manifest DTOs use names externally; `app/manifest`
resolves names → ids on parse and ids → names on render. Slug edit
isn't yet implemented; when added it needs a referencing-rewriter
(`docs/roadmap.md` C5).

`pkg/ids` (UUIDv7) and `pkg/slug` (slugify + collision suffix) are
pure helpers.

### Adapter dispatch lives on HostBinding, not Host

A single Host (e.g. AWS Bedrock) can serve models that speak different
wire protocols — Claude (Anthropic shape) and Llama (OpenAI shape).
The dispatch key therefore lives on the per-`(Model, Host)` binding,
not on the Host. `app/adapter.Kind` is `openai | anthropic` today;
new kinds correspond to new `app/api/<kind>` packages.

Ollama uses `Adapter: openai` (it exposes an OpenAI-compatible
endpoint).

### Admin CRUD surface

Eight kinds, uniform shape. **No `/control/` prefix** — the control
plane runs on its own listener (`RELAY_CONTROL_PORT`).

```
GET    /{plural}                 list
GET    /{plural}/{ref}           read by slug or id (UUID form prefers id)
POST   /{plural}                 create — server stamps id+slug from displayName
PUT    /{plural}/by-id/{id}      update (id-routed; body may change slug)
DELETE /{plural}/by-id/{id}      delete (id-routed)
```

Plurals: `providers`, `hosts`, `models`, `host-keys`, `rate-limits`,
`policies`, `pricings`, `relay-keys`.

Plus:

- `POST /auth/login`, `POST /auth/logout`, `GET /auth/whoami`
- `POST /master-key/generate`, `POST /reload`, `GET /version`

Handlers live in `app/httpapi/control/`. The generic CRUD factory is
inline in `app/httpapi/control/crud.go` — no separate `pkg/admin/crud`.

HostKey supports two value modes:
- `valueFrom: {kind: env, env: VAR_NAME}` — env-ref, no creds in PG
- `valueFrom: {kind: stored}` + `value: sk-...` — AES-GCM-256 encrypted
  with `RELAY_MASTER_KEY`, ciphertext stored in PG

Every write triggers a PG NOTIFY; the listener fans out to all pods
within ~1s (debounce window). **No manual `/reload` needed** for CRUD
operations — `/reload` is a manual full-rebuild fallback.

### Auth surface

| Caller | Endpoint surface | Auth |
|---|---|---|
| Browser → control API | `/auth/*`, CRUD, `/master-key/*`, `/reload`, `/version` | scs session cookie (`relay_session`, HttpOnly + SameSite=Strict + Secure-toggleable) |
| Operator / CI → control API | same | `Authorization: Bearer ${RELAY_ADMIN_TOKEN}` (break-glass; coexists with sessions) |
| Customer code → inference API | `/v1/*` | `Authorization: Bearer ${relay-key}`; hashed → `snapshot.RelayKeyByHash` |

Sessions are real (not "cookie value = env var" like the pre-cutover
shim). Backed by `alexedwards/scs/v2` over `kv.Store` (`app/session`),
opaque tokens rotated on login, server-side destroy on logout.

Passwords: bcrypt-aware. `internal/identity.Verify` checks `$2a$/$2b$
/$2y$` prefixes; falls back to plain compare with a deprecation log
for legacy YAML.

Future-proof seams (already wired; do not bypass):

- `app/actor.Actor` carries `UserID`, `Username`, `SessionID`,
  `AdminToken` flag in `context.Context`. Reserved `ActiveOrgID` +
  `Roles` slots for multi-tenant work.
- `app/authz.Authorizer` interface — every CRUD/mutation handler
  routes through `d.Authz.Authorize(ctx, action, resource)`. v1 impl
  `AlwaysAllowAuthenticated` returns nil for any authenticated caller.
  Swapping in Casbin / OpenFGA later is an implementation change, not
  a handler rewrite. **Do not branch handlers on user identity
  directly; always go through Authorizer.**

### Hot-path rules (non-negotiable)

- **No Postgres calls** on the request path. Catalog reads come from
  the in-memory `app/catalog.Snapshot` only.
- **Default rich parsing**: full body parse to a typed shape-specific
  struct via `pkg/api/<shape>.Parse`. `RELAY_RICH_PARSING=off` reverts
  to minimal parse (model/stream/user/raw). `messages` content is
  never deep-parsed; raw body is retained for byte-equivalent upstream
  forward.
- **Pure shape stays vendorable**: `pkg/api/<shape>` has zero Relay
  imports. Relay-specific glue (`Call`, `Retryable`, `ExtractTokens`)
  lives in `app/api/<shape>`.
- **Token counts come from the provider response** — no relay-side
  tokenisation. The `Adapter.ExtractTokens([]byte)` contract.
- **One Redis Lua call per request** is the goal: rate-limit reserve
  in a single script. Auth + key selection + circuit-breaker check
  may sit alongside it. Not three round-trips.
- **No mid-stream failover** — failover happens pre-first-byte. After
  bytes flow back to the caller, errors stop being relay's problem.
- **No middleware/plugin chain** à la LiteLLM. The handler builds a
  `pipeline.Request` and calls `Pipeline.Run`; that's the chain.
- **All emits are async**: usage → ClickHouse, span → OTel, payload →
  S3. Bounded channels with drop-on-full and a Prometheus drop
  counter. Never unbounded queues. Never block-on-send on the hot
  path.
- **Cross-shape translation is disabled** in v1. A request hitting
  `/v1/chat/completions` for a Model whose binding declares
  `Adapter: anthropic` returns 400. Same the other way. Re-enabling
  is a future slice (`docs/roadmap.md` track D).

### Post-flight contract

Per-request post-flight work (`Limiter.Commit`,
`Selector.RecordSuccess`, `OnSuccess(tokens, keyHash)`) runs in a
**detached goroutine** triggered when the caller `Close()`s
`Result.Body`. It MUST NOT block the response. Track via
`relay_pipeline_post_flight_duration_seconds` once metrics land.

### `pkg/kv` consumer conventions

1. Hash-tag every kv key with `{tag}:` where tag is the consumer's
   atomicity boundary.
2. All keys touched in one Lua script (`RunScript`) or `WithLock`
   must share the same `{tag}` substring.
3. Centralise key construction in a `keys.go` per consumer — no inline
   key strings at call sites.
4. Each consumer declares its own narrow interface listing only the
   kv methods it uses; don't import a fat `kv.Store` everywhere.
5. Every key must have a TTL unless justified in a code comment.
   Persistent data goes in Postgres.
6. Tests must pass against both `kv.Mem` and `kv.Redis` backends.
7. Document expected kv ops per request in the consumer's package doc
   comment.

Full guide: `docs/kv.md`.

### Storage layer conventions

`internal/storage` owns the Postgres pool + migrations. Post-cutover
it's a thin composition-root concern — domain code under `app/`
constructs its own sqlc queries via `gen.New(pool)` against
`storage.Pool()`.

1. No `pgx`, `pgxpool`, or `pgconn` types in any signature outside
   `internal/storage` and `app/X.Store` constructors.
2. No SQL strings (`SELECT`/`INSERT`/`UPDATE`/`DELETE`) in `.go` files
   outside `internal/storage/gen/queries.sql` and the per-entity
   `app/X.Store` files (which use sqlc-generated typed methods, not
   string SQL).
3. No sqlc-generated types in exported signatures of `app/X.Store` —
   convert to domain types (`*provider.Provider` etc.) at the
   boundary.
4. Encryption policy lives in domain (`app/hostkey`), primitives in
   `pkg/crypto`. Storage handles already-encrypted bytes; the master
   key never enters `internal/storage`.

The legacy "Storage.Catalog domain repos" pattern is GONE. Each
`app/X.Store` is independent.

Full guide: `docs/storage.md`.

### Cluster mode

`RELAY_CLUSTER_MODE=on` is on by default in any multi-pod deployment.
Catalog NOTIFY/LISTEN keeps every pod's snapshot in sync within ~1s.
Future: leader election, Redis Cluster client. The NOTIFY emit (on
catalog write) is unconditional; only the LISTEN consumer is gated.

Full guide: `docs/cluster.md`.

### Streaming

- Tee model: response bytes pass through to the caller; the post-
  flight goroutine sees a buffered copy via `io.TeeReader` once
  `Body.Close()` fires.
- Same-shape passthrough is the 95% case. Cross-shape transform is
  off-path in v1.

### Key pooling

- Failover + load-balanced within a single tenant.
- Per-key Redis circuit breakers (`FailureAuth`, `FailureRateLimit
  Short`, `FailureRateLimitLong`, `FailureServerError`, `FailureNetwork`).
- Selection algos: `prioritized` (cost-tiered first-healthy),
  `round-robin`, `least-recently-used`. Quota-aware weighted-random
  is roadmap C3.
- No cross-tenant pooling.
- Atomic via Redis Lua across all candidate keys.

### Batch

- Relay primitive, not a provider feature — works for any upstream.
- Use provider batch APIs where available (50% discount passthrough);
  simulate otherwise via a worker pool.
- Customer interface: submit → poll OR webhook → fetch from S3.
- **Not implemented yet.** Roadmap track D.

### Observability

- OTel for traces (one span per request, rich attributes including
  decision trace).
- Prometheus for pod metrics.
- ClickHouse internal for analytics.
- Structured JSON logs to stdout.

**Status**: scaffolding (`internal/usage`, `pkg/eventlog`) exists but
is no longer wired into the hot path after the cutover. Rebuild is
roadmap A2 — `app/pipeline.OnSuccess` fires; nothing emits yet.

## Performance contract

Two regimes — be precise about which one you're quoting:

**Live distributed deployment** (real Redis Lua RTT, real PG, two-pod
fleet, nginx LB):
- p50 overhead: < 2 ms
- p99 overhead: 10 ms (internal SLO), 15 ms (public claim)

**In-process bench** (in-memory `kv.Mem`, no network):
- p50 overhead: < 100 µs
- p99 overhead: < 500 µs

The 18× gap between the two is unavoidable I/O — Redis Reserve Lua
RTT, nginx hop, container network, retry/circuit logic against real
Redis. Use bench numbers as a regression gate (architecture's lower
bound); use live numbers for SLO conversations.

- RPS per pod: 5–10k.
- Tier-3 totals via horizontal scale, not per-pod heroics.
- **Post-flight runs in a detached goroutine — never blocks the
  response.**

**Status**: bench harness was deleted alongside legacy pipeline in
stage 5; new in-process bench against `app/pipeline.Pipeline.Run` is
roadmap A3. We're temporarily flying blind on regressions.

## Code style and conventions

- Go 1.25+.
- Module: `github.com/wyolet/relay`.
- Hot-path code must be allocation-conscious. Use `sync.Pool` for
  buffers; avoid string conversions; reuse header maps.
- `GOMEMLIMIT` and tuned `GOGC` are part of the deployment story.
- Async work uses bounded channels with explicit drop-on-full and a
  Prometheus drop counter — never unbounded queues, never block-on-
  send on the hot path.
- gRPC is reserved for **internal** control-plane ↔ data-plane traffic
  only. The customer-facing edge is HTTP/JSON.
- Comments: default to NONE. Write a comment only when the WHY is
  non-obvious — a hidden constraint, an invariant, a workaround for
  a specific bug, behaviour that would surprise a reader. Don't
  narrate what code does; well-named identifiers cover that.
- New packages get a top-of-file doc comment that explains intent,
  scope, and what's deliberately out of scope.

## Workflow conventions

- One stage = one PR. Don't pile unrelated changes onto a single
  branch.
- Branch off `main` after the previous PR merges; delete merged
  branches (locally + remote) immediately.
- Subagents handle mechanical / scoped work; spawn with `model:
  "sonnet"` for grunt-level tasks. Orchestration / design work stays
  on the parent.
- Smoke tests run against `deploy/compose/docker-compose.test.yml` —
  ephemeral pg on `127.0.0.1:5499`, brought up via `make
  test-integration`.

## When in doubt

- Don't over-engineer. Push back when a feature isn't earning its
  complexity.
- Boring choices on the edges, smart choices in the middle. The router
  (chi) is the edge; the pipeline is the middle.
- "Three similar lines is better than a premature abstraction."
- Read the Obsidian docs (`~/Documents/Obsidian Vault/Projects/Relay/`)
  before proposing architecture changes.
- The roadmap (`docs/roadmap.md`) names every known follow-up — check
  it before opening a new design question.
