# CLAUDE.md

Guidance for Claude Code working in this repo.

## Dev workflow

Before any non-trivial change, read `CONTRIBUTING.md` (build/test, the
PR workflow, and the load-bearing codebase rules) and the design docs
under `design/` (`design/roadmap.md` for what's next, `design/canonical-protocol.md`
for the protocol invariants). The repo layout, ports, and build/deploy
pipeline are documented in `CONTRIBUTING.md` and the `Makefile`.

## What this is

Wyolet Relay — a high-throughput LLM router in Go. Self-hostable,
k8s-native, BYO provider keys. The wedge is the infrastructure axis —
performance, key pooling, batch orchestration, observability.

Open source (Apache-2.0), published as a Docker image (`wyolet/relay`)
and deployable to k8s. The public-facing surface (README, quickstart,
issue templates, docs at docs.wyolet.com) assumes no internal context;
keep that audience in mind when editing anything outside `design/`.

Roadmap and stage breakdown: `design/roadmap.md` (the index; the OSS
core backlog lives in `design/roadmap-oss.md`). Design context lives in
the `design/` tree (`design/canonical-protocol.md`, `design/adapters/`,
the per-subsystem guides) — read before non-trivial architectural
suggestions.

## Wedge — what we are NOT

- Not a marketplace / reseller of provider tokens (deferred indefinitely)
- Not Python — performance is a wedge, runtime cost matters
- Not a feature-parity clone of an existing router — infra-grade
  throughput is the goal, not matching every checkbox
- Not adaptive / quality-aware routing in v1 — bad auto-routing loses
  customers
- Not a custom wire protocol — OpenAI/Anthropic shapes are accepted as
  passthrough

## Repository layout

Three Go modules wired by a root `go.work` (`use .`, `use ./sdk`,
`use ./jobq`). The codebase is organised by responsibility.

```
app/                       — the application: domain + composition + handlers
  catalog/                 — composition layer: immutable Snapshot, COW
                             reconciler, NOTIFY listener, Bootstrap();
                             overlay merge applied here (overlay_apply.go)
  catalogview/             — PG-backed consumer read projections for admin/UX
                             endpoints (NOT the hot-path snapshot)
  catalogembed/            — composes manifest YAML → SDK catalog embed schema
  catalogvalidate/         — cross-entity graph linter for the catalog
  {provider,host,model,
   hostkey,ratelimit,
   policy,pricing,binding,
   relaykey}/              — 9 entity packages. Each: domain types, Validate(),
                             Store{List,Get,Upsert,Delete}. `binding` is the
                             first-class HostBinding join (Model×Host×adapter).
  overlay/                 — catalog overlays: user-owned sparse spec patches
                             on pristine TEMPLATE rows, merged to EFFECTIVE
                             rows at snapshot load (survives re-seed). See
                             design/overlays.md.
  modelref/                — parses the catalog model-reference DSL + aliases
  meta/                    — identity primitives every entity carries (id/
                             name/displayName, owner)
  settings/                — typed-sectioned config layer (DB-backed settings,
                             incl. governance:* sections). See design/settings.md.
  settingswatch/           — applies a value-typed settings section to a live
                             component on change
  adapter/                 — generic adapter framework (singular):
                             Spec, Registry, generic pipeline.Adapter
                             parameterised by upstream URL + auth strategy.
                             ONE Spec literal per wire shape lives in
                             cmd/relay/main.go.
  adapters/                — vocabulary only: Name constants
  pipeline/                — pure orchestration: reserve → pick key →
                             Adapter.Call → stream → post-flight emit
  proxy/                   — second inference flow: transparent forwarding
  routing/                 — snapshot lookup → Plan{Model, Policy, Binding,
                             Host, Keys, Rules}; resolves aliases
  keypool/                 — key selection + per-key circuit breaker
                             (state at `secret_health:*` in kv)
  hosthealth/              — per-host runtime reachability (kv-backed)
  ratelimit/               — RateLimit entity + Resolve(policy, rl) → Rules
  secret/                  — secret Store + KeyAgent (out-of-band heal/failover)
  httpapi/                 — the HTTP layer
    inference/             — data plane: /v1/* + /healthz; shape-agnostic
                             Dispatch with NO per-vendor branching
    control/               — admin plane: /auth/* + CRUD + /version + ...
  transport/ws/            — customer-facing WebSocket inference transport
  batch/                   — batch consumer: reuses Pipeline.Run (source="batch")
                             over the jobq module. See design/ + roadmap.
  manifest/                — YAML DTOs + translate ↔ domain
  seed/                    — YAML → Postgres orchestration
  session/                 — scs wrapper, kv-backed
  authz/                   — Authorizer interface (v1: AlwaysAllowAuthenticated)
  actor/                   — Actor in context (user/admin-token)
  usagelog/                — lifecycle observer → bounded Emitter → usage Sink
  payloadlog/              — lifecycle observer → request/response payload store
  metricslog/              — lifecycle observer → Prometheus

pkg/                       — server-internal shared libs (NOT the SDK)
  ratelimit/               — Limiter (kv-backed Lua) + Rule + Reservation
  kv/                      — Store interface + Mem + Redis backends
  lifecycle/               — per-request Context + PreFlightMiddleware +
                             PostFlightHook + Registry. The observability
                             spine: runners build a Context, observers
                             register hooks, FirePostFlight fans out.
  usage/                   — usage Event record + query/eval engine
                             (filter/summarize/bucketize) + Sink/Reader.
    {clickhouse,file,
     postgres,valkey}/     — usage sinks (heavy deps: ch/pgx/redis). Stay
                             server-side. (Pure wire shapes — Tokens, timing —
                             live in sdk/usage; pkg/usage.Event embeds them.)
  payload/                 — request/response payload model (the /logs path)
  filter/                  — declarative allowlist query engine for list
                             endpoints (typed-accessor SOT). design/filtering.md.
  crypto/                  — AES-GCM helpers (master-key)
  secret/                  — unified secret resolution: Ref{Kind,Env,ID,Path}
                             + Resolver/Registry/Writer. Built-in env + stored
                             (AES-GCM in PG). External FETCH-ONLY backends as
                             subpackages (in-memory, never persisted): aws,
                             azure, gcp, bitwarden, onepassword (//go:build cgo;
                             excluded from default CGO_ENABLED=0 builds).
  transport/               — shared transport helpers
  httpmw/, httpheader/     — net/http middleware + header helpers
  ids/, slug/              — UUIDv7, slug minting + collision suffixes
  metrics/                 — Prometheus registry + counters
  reqid/                   — request-id middleware

sdk/                       — SEPARATE Go module (github.com/wyolet/relay/sdk):
                             the public, vendorable client library. Server
                             module depends on it (replace ./sdk + root
                             go.work); direction is server → sdk, NEVER reverse.
                             Imports nothing from app/ or internal/.
  v1/                      — CANONICAL protocol: types, Translator interface,
                             Name + Registry, 6-event streams. Imports nothing.
  adapters/openai/         — OpenAI vendor adapter (all wire shapes as files:
                             CC, Responses, Embeddings)
  adapters/anthropic/      — Anthropic vendor adapter
  adapters/gemini/         — Gemini native generateContent adapter
  oauth/                   — vendor-neutral OAuth 2.0 flow helpers (subscription
                             upstream auth; see design + secrets notes)
  {provider,host,model}/   — catalog entity wire shapes for SDK consumers
  usage/                   — pure wire shapes: Tokens + per-request timing
  catalog/                 — go:embed'd flattened catalog + model-ref resolver
                             + per-binding Cost. catalog.json is generated
                             (cmd/catalog-embed).
  client/                  — the public client: Relay(), For(ref), Generate/
                             GenerateStream, Cost(), WS.
  internal/                — sdk-private helpers

jobq/                      — SEPARATE Go module: self-contained, durable
                             background-job engine (PG store + PayloadStore;
                             "PG never holds bytes"). River-style claim model.
                             app/batch is its consumer.

internal/                  — composition root / boundary
  config/                  — RELAY_* env parsing
  identity/                — YAML-loaded users (Verify with bcrypt-or-plain)
  storage/                 — pgx pool + migrations; exposes Pool() only

cmd/relay/                 — the binary entrypoint AND the ONLY place
                             where vendor names appear in code form
cmd/catalog-validate/      — schema-validates the catalog repo's data tree
cmd/catalog-schemas/       — regenerates JSON Schemas for catalog kinds
cmd/catalog-embed/         — composes the public catalog → sdk/catalog/catalog.json
cmd/modelsdev-import/      — fetches models.dev data → manifest YAMLs (catalog source)
cmd/icon-import/           — downloads provider icons (data-source tool)
cmd/payload-migrate/       — backfills payload-log records across backends
cmd/relay-stats/           — usage/stats CLI

config/                    — relay-local YAML (NOT the public catalog)
  ratelimits/system.yaml   — relay-internal admission/DoS rules
  users/                   — deployment-specific admin credentials

migrations/postgres/       — versioned SQL up + down
deploy/compose/            — dev pg, test pg, smoke stack
design/                    — design + runbook + roadmap
integration/               — make test-integration + make smoke-mock
```

## Public catalog lives in a separate repo

The provider/host/model/pricing/policy YAMLs are NOT in this repo. They
live in `wyolet/relay-catalog` (sourced from models.dev via
`cmd/modelsdev-import`, then curated). A flattened, generated default is
`go:embed`'d into the relay binary (`sdk/catalog/catalog.json`, via
`cmd/catalog-embed`) for airgapped / first-boot scenarios. Tarball-by-tag
distribution is the planned upgrade path.

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
the parent prefix (e.g. `pricing/gpt-4o.yaml`), but `metadata.name` still
carries the full slug for cross-refs. Schema is versioned by `apiVersion`;
bumping the relay's schema means cutting a matching tag in the catalog repo.

When you need a sample YAML to reason about, check `wyolet/relay-catalog`'s
`main` branch. Do NOT regenerate them in this repo.

**Local consumption:**

- `RELAY_CONFIG_DIR` (default `config`) — relay-internal yamls only.
- `RELAY_CATALOG_DIR` — local clone of `wyolet/relay-catalog`'s `data/`
  tree. When set + `RELAY_AUTO_SEED_IF_EMPTY=1` + PG is empty, Bootstrap
  walks it recursively and seeds. Unset disables auto-seed (falls back to
  the embedded default).
- The recursive `manifest.LoadDir` is layout-agnostic — dispatches on
  each YAML's `kind` field, so the nested catalog tree just works.

## Codebase rules (non-negotiable)

These are the load-bearing rules every change must obey. Authoritative
source: `design/canonical-protocol.md` "Codebase rules" section (11 rules).
Condensed here because new sessions must inherit them.

1. **Canonical knows nothing.** `sdk/v1/` declares its own types,
   `Translator` interface, `Name` + `Registry`, and nothing else. Zero
   imports of `app/`, `internal/`, or any `sdk/adapters/<vendor>/`.
2. **Vendors import canonical.** Each `sdk/adapters/<vendor>/` imports
   `sdk/v1/` and implements its `Translator`. Vendor adapters never
   import each other.
3. **One folder per vendor, not per wire shape.** `sdk/adapters/openai/`
   owns all OpenAI wire shapes (CC, Responses, Embeddings) as files.
   Wire-shape names never appear in folder paths.
4. **No vendor names in `app/` code.** Enforceable by
   `grep -rE "openai|anthropic|gemini|cohere" app/ --include="*.go"` —
   should return only catalog data string lookups, error messages, URL
   paths, and (for the composition root) `cmd/relay/main.go`. Dispatch,
   routing, pipeline, registry, http-mw, inference — none branch on or
   import a vendor.
5. **Composition root is the only place vendor names appear in code.**
   `cmd/relay/main.go` builds `adapter.Spec` literals; every other binary,
   test, or service consumes adapters via the `Registry`.
6. **Adapters are stateless pure transforms.** A `Translator` is six
   methods (parse/serialize × request/response + the two stream
   factories). No per-request state on the `Translator` value — per-stream
   state lives in the closures the stream factories return.
7. **`extensions` envelope for cross-cutting concerns.** Vendor-specific
   features that don't map cleanly (safety settings, RAG documents) live
   in `Request.Extensions` / `Response.Extensions`. No new top-level
   canonical field for vendor-specific features. **Exception — prompt
   caching is first-class** (`Request.CacheConfig` + per-item
   `ItemCacheConfig{anchor}`): a clean cross-vendor intent, not a vendor
   quirk. No vendor cache vocab reaches canonical; hit-rate reads back via
   `Usage["cache_read"]`.
8. **`provider_data` for same-vendor opaque blobs.** Signed/encrypted
   vendor payloads (Anthropic thinking signatures, OpenAI
   `encrypted_content`) ride on the relevant item as `json.RawMessage`.
   Round-tripped verbatim within a vendor; dropped cross-vendor.
9. **Refusal is a stop_reason, not an item type.** Refusal text appears
   as a normal `message` item with `finish_reason: "refusal"`. No
   `refusal_part` type.
10. **SDK module purity preserved.** The `sdk/` module imports nothing
    from `app/` or `internal/` — standalone, vendorable. Server → sdk,
    never reverse. (`pkg/` likewise imports nothing from `app/`/`internal/`.)
    The catalog crosses the boundary as a generated `go:embed`'d JSON, not
    an import.
11. **No silent drops.** An adapter must never accept canonical input (or
    upstream output) it can't express and discard it silently. Either emit
    it, carry it in `provider_data`/`extensions`, annotate an irreducible
    drop with a greppable `// canonical: <field> dropped — <why>` comment,
    or — for safety-relevant signals (unmapped finish/stop reason, content
    filter, refusal) — surface it rather than masquerade as success.

The grep tests for rules 1, 2, 4, 10 must hold on every commit.

---

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

### Domain model

```
Provider (vendor display)
Host     (serving endpoint)
HostKey  (upstream credential, owner = Host)
RateLimit (rule set)

Policy (hub) ── ModelIDs ── Model
          ├── HostKeyIDs
          └── RateLimitID

Binding (first-class join: ModelID × HostID)
          ├── Adapter (openai | anthropic | gemini | …)
          ├── UpstreamName
          ├── PricingID (optional)
          └── Enabled

Pricing  (owner=Host, applied to N bindings, tier-aware via AboveTokens)
RelayKey (inbound customer API key → PolicyID)
```

HostBinding is its own entity (`app/binding`), not an array embedded in
`model.Spec`. The promotion gives pricing and routing a real addressable
row, and lets one Model be served by many Hosts (incl. aggregators
re-serving another provider's model). It's a join owned by no single side
(Owner is system-kind); `(ModelID, HostID)` is unique (DB constraint +
catalog re-check). Routing reads `snapshot.BindingsForModel(modelID)`.

**Route entity is deferred** — Policy + RelayKey cover the v1 case. When
multi-tenancy lands the Route + Org/Project hierarchy comes back per
`design/roadmap.md`.

### Identity model (every catalog resource)

Three-field identity (`app/meta`), each with a distinct role:

- `metadata.id` — UUIDv7, **immutable**, server-stamped on create. PG
  primary key. Used in all id-routed admin URLs.
- `metadata.name` — DNS-1123 slug, stable, mutable. Auto-derived from
  `displayName` on create with a collision suffix. Used in human-readable
  URLs and YAML refs.
- `metadata.displayName` — free text. Edits are free; nothing references it.

Cross-references in spec fields store **ids**, not slugs. YAML manifest
DTOs use names externally; `app/manifest` resolves names → ids on parse
and ids → names on render. `pkg/ids` (UUIDv7) and `pkg/slug` (slugify +
collision suffix) are pure helpers.

### Model resolution + aliases

Model references resolve through `app/modelref` (the catalog reference
DSL). Resolution-only **aliases** are last-priority matchers attached to a
Model (`spec.aliases`, e.g. `gpt-5-5[1m]`, wildcard `[*]`); a matched
alias routes to that model but the upstream model name is sent verbatim.
See `design/model-aliases.md`.

### Adapter dispatch lives on Binding, not Host

A single Host (e.g. AWS Bedrock) can serve models that speak different
wire protocols — Claude (Anthropic shape) and Llama (OpenAI shape). The
dispatch key therefore lives on the per-`(Model, Host)` Binding.
`Binding.Adapter` is `openai | anthropic | gemini` today; new vendors land
as a new `sdk/adapters/<vendor>/` package implementing `v1.Translator`
plus a `Spec` registration in `cmd/relay/main.go`.

Ollama uses `Adapter: openai` (OpenAI-compatible endpoint).

OpenAI wire shapes are registered as separate Specs sharing the same
vendor package: `openai` (CC), `openai_responses` (Responses API),
`openai_embeddings` (Embeddings — `BytePass: true`). The Spec carries
inbound URL paths, upstream URL path, auth strategy, translator, optional
`IsNativePath` predicate (byte-pass when host == OpenAI proper).

### Admin CRUD surface

Nine kinds, uniform shape. The control API is mounted under **`/api`** on
the control listener (`RELAY_CONTROL_PORT`). The prefix exists because the
embedded SPA is served from the *same* listener (as the `NotFound`
fallback), and its client-side routes would otherwise be shadowed by the
identically-named CRUD endpoints on a hard reload. `/config.json` and
`/metrics` stay at the listener root. The UI learns the prefix at runtime
from `config.json`'s `controlApiUrl` (defaults to `/api`).

```
GET    /api/{plural}                 list
GET    /api/{plural}/{ref}           read by slug or id (UUID form prefers id)
POST   /api/{plural}                 create — server stamps id+slug from displayName
PUT    /api/{plural}/by-id/{id}      update (id-routed; body may change slug)
DELETE /api/{plural}/by-id/{id}      delete (id-routed)
```

Plurals: `providers`, `hosts`, `models`, `host-keys`, `rate-limits`,
`policies`, `pricings`, `host-bindings`, `relay-keys`.

Plus:

- `POST /auth/login`, `POST /auth/logout`, `GET /auth/whoami`
- `POST /master-key/generate`, `POST /reload`, `GET /version`
- per-kind sub-resources (e.g. `host-keys/by-id/{id}/health`, `.../rotate`,
  `policies/by-id/{id}/relay-keys/{relayKeyId}`) and read projections from
  `app/catalogview`.

Handlers live in `app/httpapi/control/`. The generic CRUD factory
(`registerKind[T]`) is inline in `app/httpapi/control/crud.go`. The HTTP
layer uses huma (mind its footguns: unexported fields are skipped, no
pointer query params).

HostKey supports two value modes:
- `valueFrom: {kind: env, env: VAR_NAME}` — env-ref, no creds in PG
- `valueFrom: {kind: stored}` + `value: sk-...` — AES-GCM-256 encrypted
  with `RELAY_MASTER_KEY`, ciphertext stored in PG

Every write triggers a PG NOTIFY; the listener fans out to all pods within
~1s. **No manual `/reload` needed** for CRUD — `/reload` is a manual
full-rebuild fallback.

### Auth surface

| Caller | Endpoint surface | Auth |
|---|---|---|
| Browser → control API | `/auth/*`, CRUD, `/master-key/*`, `/reload`, `/version` | scs session cookie (`relay_session`, HttpOnly + SameSite=Strict + Secure-toggleable) |
| Operator / CI → control API | same | `Authorization: Bearer ${RELAY_ADMIN_TOKEN}` (break-glass; coexists with sessions) |
| Customer code → inference API | `/v1/*` | `Authorization: Bearer ${relay-key}`; hashed → `snapshot.RelayKeyByHash` |

Sessions are real, backed by `alexedwards/scs/v2` over `kv.Store`
(`app/session`), opaque tokens rotated on login, server-side destroy on
logout. Passwords are bcrypt-aware (`internal/identity.Verify`; plain
fallback with a deprecation log for legacy YAML).

Future-proof seams (already wired; do not bypass):

- `app/actor.Actor` carries `UserID`, `Username`, `SessionID`,
  `AdminToken` in `context.Context`. Reserved `ActiveOrgID` + `Roles`
  slots for multi-tenant work.
- `app/authz.Authorizer` — every CRUD/mutation handler routes through
  `d.Authz.Authorize(ctx, action, resource)`. v1 impl
  `AlwaysAllowAuthenticated`. Swapping in Casbin / OpenFGA later is an
  implementation change, not a handler rewrite. **Do not branch handlers
  on user identity directly; always go through Authorizer.**

### Settings + governance

`app/settings` is the typed-sectioned, DB-backed config layer (sections
seeded from YAML, mutable via control API, watched live by
`app/settingswatch`). Resource edit/delete governance lives in
`governance:*` sections holding `Governance{allowEdit, allowDelete}`
(default edit:true / delete:false — a speed-bump, not a wall). Owner-tier
invariants are hardcoded in `app/settings.Check(op, kind, ownerKind)`
(system rows never delete/edit-via-CRUD; user rows allowed;
catalog-managed rows consult the section). See `design/settings.md`.

### Hot-path rules (non-negotiable)

- **No Postgres calls** on the request path. Catalog reads come from the
  in-memory `app/catalog.Snapshot` only. (`app/catalogview` is the PG-backed
  read projection for *admin* endpoints — never the hot path.)
- **Pure shape stays vendorable**: `sdk/adapters/<vendor>/` has zero `app/`
  or `internal/` imports. The vendor adapter's `pipeline.Adapter` is generic
  (`app/adapter.specAdapter`), parameterised by upstream URL + auth strategy.
- **Token counts come from the provider response** — no relay-side
  tokenisation. The `Adapter.ExtractTokens([]byte)` contract takes the full
  response buffer (streaming usage may live only in `message_start`).
- **One Redis Lua call per request** is the goal: rate-limit reserve in a
  single script. Not three round-trips.
- **No mid-stream failover** — failover happens pre-first-byte. After bytes
  flow back to the caller, errors stop being relay's problem.
- **No middleware/plugin chain.** The handler builds a `pipeline.Request`
  and calls `Pipeline.Run`; that's the chain.
- **All emits are async**: usage → ClickHouse, payload → store, metrics →
  Prometheus. Bounded channels with drop-on-full and a Prometheus drop
  counter. Never unbounded queues. Never block-on-send on the hot path.
- **Cross-shape translation** runs against the relay-native canonical
  (`sdk/v1/`). Composition handles any A→B route — no pairwise packages.
  Same-shape (or `IsNativePath` match) byte-passes via `io.Copy`;
  `BytePass: true` shapes (Embeddings) never translate. `app/httpapi/inference/`
  is shape-agnostic; route mounting is generic via
  `inference.MountRegistry(specRegistry)`.

### Post-flight contract

Per-request post-flight work (`Limiter.Commit`, `Selector.RecordSuccess`,
then `Lifecycle.FirePostFlight`) runs in a **detached goroutine** triggered
when the caller `Close()`s `Result.Body`. It MUST NOT block the response.
Observers are `lifecycle.PostFlightHook`s on the shared
`lifecycle.Registry`; each runs in its own sub-goroutine with per-hook
panic recovery. Hooks read the per-request `lifecycle.Context` and the
`PostFlightEvent`; they never mutate.

### `pkg/kv` consumer conventions

1. Hash-tag every kv key with `{tag}:` where tag is the consumer's
   atomicity boundary.
2. All keys touched in one Lua script (`RunScript`) or `WithLock` must
   share the same `{tag}` substring.
3. Centralise key construction in a `keys.go` per consumer — no inline key
   strings at call sites.
4. Each consumer declares its own narrow interface listing only the kv
   methods it uses; don't import a fat `kv.Store` everywhere.
5. Every key must have a TTL unless justified in a code comment. Persistent
   data goes in Postgres.
6. Tests must pass against both `kv.Mem` and `kv.Redis` backends.
7. Document expected kv ops per request in the consumer's package doc.

Full guide: `design/kv.md`.

### Storage layer conventions

`internal/storage` owns the Postgres pool + migrations. Domain code under
`app/` constructs its own sqlc queries via `gen.New(pool)` against
`storage.Pool()`.

1. No `pgx`/`pgxpool`/`pgconn` types in any signature outside
   `internal/storage` and `app/X.Store` constructors.
2. No SQL strings outside `internal/storage/gen/queries.sql` and the
   per-entity `app/X.Store` files (which use sqlc-generated typed methods).
3. No sqlc-generated types in exported signatures of `app/X.Store` —
   convert to domain types at the boundary.
4. Encryption policy lives in domain (`app/hostkey`), primitives in
   `pkg/crypto`. Storage handles already-encrypted bytes; the master key
   never enters `internal/storage`.

Each `app/X.Store` is independent. Full guide: `design/storage.md`.

### Cluster mode

`RELAY_CLUSTER_MODE=on` is on by default in any multi-pod deployment.
Catalog NOTIFY/LISTEN keeps every pod's snapshot in sync within ~1s. The
NOTIFY emit (on catalog write) is unconditional; only the LISTEN consumer
is gated. Full guide: `design/cluster.md`.

### Streaming

- Tee model: response bytes pass through to the caller; the post-flight
  goroutine sees a buffered copy via `io.TeeReader` once `Body.Close()` fires.
- Same-shape passthrough is the 95% case. Cross-shape transform runs
  per-chunk via stateful translators (`design/adapters.md`) — each SSE event
  is parsed at the `\n\n` boundary, translated, flushed. Either side may be
  identity (no-op) when shapes match.

### Key pooling

- Failover + load-balanced within a single tenant. No cross-tenant pooling.
- Per-key Redis circuit breakers (`FailureAuth`, `FailureRateLimitShort`,
  `FailureRateLimitLong`, `FailureServerError`, `FailureNetwork`,
  `FailureUpstreamUnreachable`). Atomic via Redis Lua across all candidates.
- Selection algos: `prioritized` (cost-tiered first-healthy), `round-robin`,
  `least-recently-used`. Quota-aware weighted-random is roadmap.
- **Breaker keyed by value-hash** — a rotated key gets a new hash → a fresh
  (closed) breaker automatically; the old hash's record orphans + TTLs out.
- **Dial-vs-key failure split**: an unreachable host re-dials the same host
  (not a key failure); only 401/403 breaks a key. Anon keys are breaker-exempt.
  See `app/hosthealth`.
- **Secret failover/heal via `pipeline.KeyAgent`** (impl `app/secret.Agent`):
  on `FailureAuth` the request loop calls `KeyAgent.OnFailure` and obeys the
  verdict — fail over + heal in the background when other candidates remain,
  or (last resort) park on a single-flighted re-resolve and retry the SAME
  key with the fresh value. Revoked (value unchanged) → clean error. The
  request never imports `secret`; `keyRefresher` (cmd/relay) re-resolves via
  `hostkey.Store.Get` + heals the snapshot via `ApplyHostKeyUpsert`. Nil
  KeyAgent = legacy failover.

### Batch

Shipped. A relay primitive, not a provider feature — works for any upstream.

- `jobq` is a self-contained durable job-engine module (PG store +
  PayloadStore; PG never holds bytes). River-style claim model
  (state-flip + commit, rescuer on every node; leader election deferred).
- `app/batch` is the consumer: reuses `Pipeline.Run` with `source="batch"`,
  sharing the realtime path's Adapter + keypool Selector. Admit/execute gate
  split shares `policy.Service`.
- Uses provider batch APIs where available (discount passthrough); a worker
  pool simulates otherwise. Customer interface: submit → poll / webhook →
  fetch result.

### Observability

The lifecycle hook system (`pkg/lifecycle`) is the spine: a per-request
`Context` built at request entry by every runner (pipeline / proxy / ws /
batch), threaded through, handed to registered observers in the detached
post-flight goroutine via `Registry.FirePostFlight`. New observers are
additive — register a `PostFlightHook` at boot, no pipeline changes. Three
observers are live:

- **Usage** (`app/usagelog`): PostFlight hook → bounded `Emitter`
  (drop-on-full + atomic drop counter) → `Sink`. Sinks: file (JSONL),
  ClickHouse (prod/dev default), Postgres, valkey. Cost is computed at emit
  time (per-binding pricing; nil = unpriced ≠ 0). Read API + `cmd/relay-stats`
  consume it.
- **Payload** (`app/payloadlog`): request/response bodies → payload store
  (the `/logs` UI path). Dev default is the ClickHouse backend — the `file`
  (JSONL) backend is a disk-eating footgun (linear-scan reader, no rotation);
  never leave it on. `cmd/payload-migrate` backfills across backends.
- **Prometheus** (`app/metricslog`): request counters/histograms. See
  `design/metrics.md` and `docs/metrics.md`.

OTel traces are not wired yet — the span belongs on the lifecycle `Context`,
started at entry, ended in post-flight. Structured JSON logs go to stdout.

## Performance contract

Two regimes — be precise about which one you're quoting:

**Live distributed deployment** (real Redis Lua RTT, real PG, two-pod
fleet, nginx LB):
- p50 overhead: < 2 ms
- p99 overhead: 10 ms (internal SLO), 15 ms (public claim)

**In-process bench** (in-memory `kv.Mem`, no network):
- p50 overhead: < 100 µs
- p99 overhead: < 500 µs

The ~18× gap is unavoidable I/O (Redis Reserve Lua RTT, nginx hop, container
network, retry/circuit logic). Use bench numbers as a regression gate;
use live numbers for SLO conversations.

- RPS per pod: 5–10k.
- Tier-3 totals via horizontal scale, not per-pod heroics.
- **Post-flight runs in a detached goroutine — never blocks the response.**

## Code style and conventions

- Go 1.25+. Module: `github.com/wyolet/relay` (sdk + jobq are separate
  modules under the root `go.work`).
- Hot-path code must be allocation-conscious. Use `sync.Pool` for buffers;
  avoid string conversions; reuse header maps.
- `GOMEMLIMIT` and tuned `GOGC` are part of the deployment story.
- Async work uses bounded channels with explicit drop-on-full and a
  Prometheus drop counter — never unbounded queues, never block-on-send on
  the hot path.
- gRPC is reserved for **internal** control-plane ↔ data-plane traffic only.
  The customer-facing edge is HTTP/JSON.
- Comments: default to NONE. Write a comment only when the WHY is
  non-obvious — a hidden constraint, an invariant, a workaround, behaviour
  that would surprise a reader. Don't narrate what code does.
- New packages get a top-of-file doc comment that explains intent, scope,
  and what's deliberately out of scope.

## Workflow conventions

- One stage = one PR. Don't pile unrelated changes onto a single branch.
- Branch off `main` after the previous PR merges; delete merged branches
  (locally + remote) immediately.
- Subagents handle mechanical / scoped work; spawn with `model: "sonnet"`
  for grunt-level tasks. Orchestration / design work stays on the parent.
- Smoke tests run against `deploy/compose/docker-compose.test.yml` —
  ephemeral pg on `127.0.0.1:5499`, brought up via `make test-integration`.
- Higher-fidelity smoke: `make smoke-mock` replays recorded fixtures through
  relay → Caddy → spec-mock, validating wire-level dispatch end-to-end
  (parallel tool-calls + streaming).
- When a misconfigured upstream trips key-pool breakers, `make
  breakers-reset` clears `secret_health:*` keys in valkey.
- `make lint-rules` enforces the grep-based codebase rules; it must pass.
- Never restart the running dev relay (isolate on a spare port or ask).

## When in doubt

- Don't over-engineer. Push back when a feature isn't earning its complexity.
- Boring choices on the edges, smart choices in the middle. The router (chi)
  is the edge; the pipeline is the middle.
- "Three similar lines is better than a premature abstraction."
- Read the design docs under `design/` before proposing architecture changes.
- The roadmap (`design/roadmap.md`) names every known follow-up — check it
  before opening a new design question.
