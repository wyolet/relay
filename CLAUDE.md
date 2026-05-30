# CLAUDE.md

Guidance for Claude Code working in this repo.

## Dev workflow

Before any non-trivial change, read `CONTRIBUTING.md` (build/test, the
PR workflow, and the load-bearing codebase rules) and the design docs
under `docs/` (`docs/roadmap.md` for what's next, `docs/canonical-protocol.md`
for the protocol invariants). The repo layout, ports, and build/deploy
pipeline are documented in `CONTRIBUTING.md` and the `Makefile`.

## What this is

Wyolet Relay — a high-throughput LLM router in Go. Self-hostable,
k8s-native, BYO provider keys. Competes with OpenRouter and LiteLLM on
the infrastructure axis (performance, key pooling, batch orchestration,
observability).

Roadmap and stage breakdown: `docs/roadmap.md` (split into per-phase
docs — see the index). Design context lives in the `docs/` tree
(`docs/canonical-protocol.md`, `docs/adapters/`, the per-subsystem
guides) — read before non-trivial architectural suggestions.

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

The codebase is organised by responsibility. Canonical Phase 2 shipped
2026-05-22 (PRs #185–189); the layout below reflects post-migration
state.

```
app/                       — the application: domain + composition + handlers
  catalog/                 — composition layer: immutable Snapshot, COW
                             reconciler, NOTIFY listener, Bootstrap()
  {provider,host,model,
   hostkey,ratelimit,
   policy,relaykey,pricing}/  — 8 entity packages
                             — each: domain types, Validate(), Store{List,
                                     Get,Upsert,Delete}
  adapter/                 — generic adapter framework (singular):
                             Spec, Registry, generic pipeline.Adapter
                             parameterised by upstream URL + auth strategy.
                             ONE Spec literal per wire shape lives in
                             cmd/relay/main.go.
  adapters/                — vocabulary only: Name constants + the OLD
                             adapters.Translator interface (PR 5 deletes
                             the latter; the file is unreferenced).
  pipeline/                — pure orchestration: reserve → pick key →
                             Adapter.Call → stream → post-flight emit
  routing/                 — snapshot lookup → Plan{Model, Policy,
                             HostBinding, Host, Keys, Rules}
  keypool/                 — key selection + per-key circuit breaker
                             (state at `secret_health:*` in kv)
  ratelimit/               — Resolve(policy, rl) → []pkgratelimit.Rule
  httpapi/                 — the HTTP layer
    inference/             — data plane: /v1/* + /healthz; shape-agnostic
                             Dispatch with NO per-vendor branching
    control/               — admin plane: /auth/* + CRUD + /version + ...
  manifest/                — YAML DTOs + translate ↔ domain
  seed/                    — YAML → Postgres orchestration
  session/                 — scs wrapper, kv-backed
  authz/                   — Authorizer interface (v1: AlwaysAllow)
  actor/                   — Actor in context (user/admin-token)
  usagelog/                — lifecycle PostFlight observer → bounded
                             Emitter (drop-on-full) → Sink (JSONL today).
                             The live usage-emit path.

pkg/                       — server-internal shared libs (NOT the SDK)
  ratelimit/               — Limiter (kv-backed Lua) + Rule + Reservation
  kv/                      — Store interface + Mem + Redis backends
  lifecycle/               — per-request Context + PreFlightMiddleware +
                             PostFlightHook + Registry. The observability
                             spine: runners build a Context, observers
                             register hooks, FirePostFlight fans out.
  usage/                   — usage event record (Event) + query/eval engine
                             (filter/summarize/bucketize) + Sink/Reader. The
                             server-side root; backends live in subpackages.
    {clickhouse,file,
     postgres,valkey}/     — usage sinks (heavy deps: ch/pgx/redis). Stay
                             server-side. (Pure wire shapes — Tokens, timing —
                             live in sdk/usage; pkg/usage.Event embeds them.)
  crypto/                  — AES-GCM helpers (master-key)
  secret/                  — unified secret resolution: Ref{Kind,Env,ID,Path}
                             + Resolver/Registry/Writer. Built-in env + stored
                             (AES-GCM in PG via app/secret.Store). External
                             FETCH-ONLY backends as subpackages (in-memory,
                             never persisted): aws (stdlib SigV4), azure, gcp
                             (stdlib), bitwarden (pure-Go client crypto),
                             onepassword (official SDK, //go:build cgo — the
                             SDK needs CGO; excluded from default CGO_ENABLED=0
                             builds). Composition wires kinds in app/secret.Wire.
  httpmw/, httpheader/     — net/http middleware + header helpers
  ids/, slug/              — UUIDv7, slug minting + collision suffixes
  metrics/                 — Prometheus registry + counters
  reqid/                   — request-id middleware

sdk/                       — SEPARATE Go module (github.com/wyolet/relay/sdk):
                             the public, vendorable client library. The server
                             module depends on it (replace ./sdk + root
                             go.work); direction is server → sdk, NEVER reverse.
                             Imports nothing from app/ or internal/ — a consumer
                             `go get`s only this, no pgx/redis/clickhouse.
  v1/                      — CANONICAL protocol: types, Translator interface,
                             Name + Registry, 6-event streams. Imports nothing.
  adapters/openai/         — OpenAI vendor adapter (one folder, all wire
                             shapes as files): chat_request/parse/tokens/
                             context/types (CC), responses_*.go,
                             translator_cc.go, translator_responses.go.
  adapters/anthropic/      — Anthropic vendor adapter: parse, content, stream,
                             tokens, types, transform, translator_canonical.go.
  adapters/gemini/         — Gemini native generateContent adapter.
  usage/                   — pure wire shapes only: Tokens + per-request
                             timing (Upstream/ReasoningTiming). The Event
                             record + filter/aggregate engine live server-side
                             in pkg/usage (consumed by no SDK client).
  catalog/                 — go:embed'd flattened catalog (hosts/bindings/
                             pricing) + model-ref resolver + per-binding Cost.
                             catalog.json is generated; see cmd/catalog-embed.
  client/                  — the public client: Relay(), For(ref) catalog
                             resolution, Generate/GenerateStream, Cost(), WS.

internal/                  — composition root / boundary
  config/                  — RELAY_* env parsing
  identity/                — YAML-loaded users (Verify with bcrypt-or-plain)
  storage/                 — pgx pool + migrations; exposes Pool() only

cmd/relay/                 — the binary entrypoint AND the ONLY place
                             where vendor names appear in code form
cmd/litellm-import/        — fetches LiteLLM JSON → manifest YAMLs
cmd/catalog-validate/      — schema-validates the catalog repo's data tree
cmd/catalog-schemas/       — regenerates JSON Schemas for catalog kinds
cmd/catalog-embed/         — composes the public catalog → sdk/catalog/catalog.json
                             (server-side; imports app/seed+manifest, skips drafts)

config/                    — relay-local YAML (NOT the public catalog)
  ratelimits/system.yaml   — relay-internal admission/DoS rules
  users/                   — deployment-specific admin credentials

migrations/postgres/       — versioned SQL up + down

deploy/compose/            — dev pg, test pg, smoke stack
docs/                      — design + runbook + roadmap
integration/               — make test-integration + make smoke-mock
```

**Deleted in canonical Phase 2 (do not recreate):**

- `app/adapters/openai/` and `app/adapters/anthropic/` — Relay-side glue
  collapsed into the generic `app/adapter/` framework.
- `sdk/adapters/openai/responses/{cctranslator,anthropictranslator}/`
  pairwise translator packages — replaced by canonical chain
  composition (see docs/canonical-protocol.md).
- `Deps.CrossShapeHandlers` + `inference.CrossShapeHandler` — the
  temporary hook is gone; dispatch is uniform.
- `Deps.Translators` (the old `adapters.Registry` keyed on the
  CC-as-canonical Translator interface). `Deps.Specs *adapter.Registry`
  replaces it.

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

## Codebase rules (non-negotiable)

These are the load-bearing rules every change must obey. Authoritative
source: `docs/canonical-protocol.md` "Codebase rules" section. Quoted
here because new sessions must inherit them.

1. **Canonical knows nothing.** `sdk/v1/` declares its own types,
   `Translator` interface, `Name` + `Registry`, and nothing else. Zero
   imports of `app/`, `internal/`, or any `sdk/adapters/<vendor>/`.
2. **Vendors import canonical.** Each `sdk/adapters/<vendor>/` imports
   `sdk/v1/` and implements its `Translator`. Vendor adapters
   never import each other.
3. **One folder per vendor, not per wire shape.** `sdk/adapters/openai/`
   owns all OpenAI wire shapes (CC, Responses, Embeddings) as files.
   Wire-shape names never appear in folder paths.
4. **No vendor names in `app/` code.** Enforceable by
   `grep -rE "openai|anthropic" app/ --include="*.go"` — should return
   only catalog data string lookups, error messages, URL paths, and
   `cmd/relay/main.go`. Dispatch, routing, pipeline, registry, http-mw,
   inference — none of them branch on or import a vendor.
5. **Composition root is the only place vendor names appear in code.**
   `cmd/relay/main.go` builds `adapter.Spec` literals; every other
   binary, test, or service consumes adapters via the `Registry`.
6. **Adapters are stateless pure transforms.** A `Translator` is six
   methods (parse/serialize × request/response + the two stream
   factories). No per-request state on the `Translator` value — per-
   stream state lives in the closures the stream factories return.
7. **`extensions` envelope for cross-cutting concerns.** Anything that
   doesn't map cleanly across vendors (safety settings, RAG documents)
   lives in `Request.Extensions` / `Response.Extensions`
   (`map[string]json.RawMessage`). Vendor adapters that understand a key
   emit the corresponding wire field; adapters that don't, ignore it.
   No new top-level canonical field for *vendor-specific* features.
   **Exception — prompt caching is first-class.** It's a clean
   cross-vendor intent ("this prefix is stable"), not a vendor quirk,
   so it lives in `Request.CacheConfig` (`cache_config`: object with
   `instructions`/`tools`, mirroring `model_config`) + a per-item
   `cache_config` (`ItemCacheConfig{anchor}`). No vendor cache vocab
   (`cache_control`, `prompt_cache_key`) reaches canonical; Anthropic
   emits breakpoints, OpenAI no-ops, hit-rate reads back via
   `Usage["cache_read"]`. See `docs/canonical-protocol.md` rule 7.
8. **`provider_data` for same-vendor opaque blobs.** Signed/encrypted
   vendor payloads (Anthropic thinking signatures, OpenAI
   `encrypted_content`) carry on the relevant item (`reasoning`,
   `tool_call`, `message`) as a `json.RawMessage`. Round-tripped
   verbatim within a vendor; dropped cross-vendor.
9. **Refusal is a stop_reason, not an item type.** The model's refusal
   text appears as a normal `message` item's text content with
   `finish_reason: "refusal"` on the response. There is no
   `refusal_part` type.
10. **SDK module purity preserved.** The `sdk/` module
    (`github.com/wyolet/relay/sdk`) imports nothing from `app/` or
    `internal/` — it is a standalone, vendorable library (`v1`, `adapters/*`,
    `usage`, `catalog`, `client`). The server module depends on `sdk`, never
    the reverse. (`pkg/` likewise imports nothing from `app/`/`internal/`.)
    The catalog is delivered as a generated `go:embed`'d JSON, not an import,
    so no edge crosses the boundary.
11. **No silent drops.** An adapter must never accept canonical input (or
    upstream output) it can't express and discard it silently. Either emit
    it, carry it in `provider_data`/`extensions` (rules 7–8), annotate an
    irreducible drop with a greppable `// canonical: <field> dropped — <why>`
    comment, or — for safety-relevant signals (unmapped finish/stop reason,
    content filter, refusal) — surface it rather than masquerade as success.
    Silent accept-and-discard is the bug class the `docs/adapters/` fidelity
    audits found across every adapter. (Automated `adapter_drop` warning
    emission is deferred; translators are pure with no logger.)

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
not on the Host. `HostBinding.Adapter` is `openai | anthropic` today;
new vendors land as a new `sdk/adapters/<vendor>/` package implementing
`v1.Translator` plus a `Spec` registration in `cmd/relay/main.go`.

Ollama uses `Adapter: openai` (it exposes an OpenAI-compatible
endpoint).

Three OpenAI wire shapes are registered as separate Specs sharing the
same underlying vendor package: `openai` (CC), `openai_responses`
(Responses API), `openai_embeddings` (Embeddings — `BytePass: true`).
The Spec carries inbound URL paths, upstream URL path, auth strategy,
translator, optional `IsNativePath` predicate (used by the OpenAI
Responses spec to byte-pass when the host is OpenAI proper).

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
- **Pure shape stays vendorable**: `sdk/adapters/<vendor>/` has zero
  `app/` or `internal/` imports. Vendor adapter's `pipeline.Adapter`
  implementation is generic (`app/adapter.specAdapter`) parameterised
  by upstream URL + auth strategy from the Spec.
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
- **Cross-shape translation** runs against a relay-native canonical
  (`sdk/v1/`, narrowed Responses shape). Every vendor adapter
  implements `v1.Translator`'s six methods (parse/serialize × request/
  response + two stream factories). Composition handles any A→B route
  — no pairwise packages. When inbound shape == upstream shape (same-
  shape) or the inbound spec's `IsNativePath` matches, dispatch byte-
  passes via `io.Copy`. `BytePass: true` shapes (Embeddings) never
  translate. Otherwise the canonical chain runs. `app/httpapi/inference/`
  is shape-agnostic; route mounting is generic via
  `inference.MountRegistry(specRegistry)`.

### Post-flight contract

Per-request post-flight work (`Limiter.Commit`,
`Selector.RecordSuccess`, then `Lifecycle.FirePostFlight`) runs in a
**detached goroutine** triggered when the caller `Close()`s
`Result.Body`. It MUST NOT block the response. Observers are
`lifecycle.PostFlightHook`s registered on the shared `lifecycle.Registry`;
each hook runs in its own sub-goroutine with per-hook panic recovery.
Hooks read the per-request `lifecycle.Context` (identity + routing +
`Metadata`) and the `PostFlightEvent` (status, duration, error, response
body); they never mutate. Track via
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
- Same-shape passthrough is the 95% case. Cross-shape transform runs
  per-chunk via stateful translators (`docs/adapters.md`) — each SSE
  event is parsed at the `\n\n` boundary, translated upstream→OpenAI→
  inbound, flushed. Either side may be identity (no-op) when shapes match.

### Key pooling

- Failover + load-balanced within a single tenant.
- Per-key Redis circuit breakers (`FailureAuth`, `FailureRateLimit
  Short`, `FailureRateLimitLong`, `FailureServerError`, `FailureNetwork`).
- Selection algos: `prioritized` (cost-tiered first-healthy),
  `round-robin`, `least-recently-used`. Quota-aware weighted-random
  is roadmap C3.
- No cross-tenant pooling.
- Atomic via Redis Lua across all candidate keys.
- **Breaker keyed by value-hash** — a rotated key gets a new hash → a fresh
  (closed) breaker automatically; the old hash's record orphans + TTLs out.
- **Secret failover/heal via `pipeline.KeyAgent`** (impl `app/secret.Agent`):
  on a `FailureAuth` the dumb request loop calls `KeyAgent.OnFailure` and obeys
  the verdict — fail over now + heal the key in the background when other
  candidates remain, or (last resort) park on a single-flighted re-resolve and
  retry the SAME key with the fresh value. Revoked (value unchanged) → clean
  error. The request never imports `secret`; `keyRefresher` (cmd/relay)
  re-resolves via `hostkey.Store.Get` + heals the snapshot via
  `ApplyHostKeyUpsert`. Nil KeyAgent = legacy failover.

### Batch

- Relay primitive, not a provider feature — works for any upstream.
- Use provider batch APIs where available (50% discount passthrough);
  simulate otherwise via a worker pool.
- Customer interface: submit → poll OR webhook → fetch from S3.
- **Not implemented yet.** Roadmap track D.

### Observability

The lifecycle hook system (`pkg/lifecycle`) is the spine: a per-request
`Context` is built at request entry by every runner (pipeline / proxy /
ws / batch), threaded through, and handed to registered observers in the
detached post-flight goroutine via `Registry.FirePostFlight`. New
observers are additive — register a `PostFlightHook` at boot, no
pipeline changes.

- **Usage**: `app/usagelog` is the live observer — PostFlight hook →
  bounded `Emitter` (drop-on-full + atomic drop counter) → `Sink`.
  Today's sink is JSONL (`FileSink`/`StdoutSink`); a ClickHouse sink is
  the next observer to add.
- **OTel traces**: not wired yet — the span belongs on the lifecycle
  `Context`, started at entry, ended in post-flight. (The old no-op span
  in `reqid` + the `internal/usage` OTel provider were deleted.)
- **Prometheus**: `pkg/metrics` declares request counters/histograms;
  wiring them onto the lifecycle hook (incl.
  `relay_pipeline_post_flight_duration_seconds`) is pending.
- Structured JSON logs to stdout.

**Deleted (pre-cutover generation, do not recreate):** `internal/usage`
(`Record`/`Init`/OTel provider + token/cost counters), `pkg/eventlog`
(file + ClickHouse `Logger`), `Request.OnSuccess` on pipeline + proxy,
and the `X-Relay-Metadata` attribution header (superseded by the
`X-WR-*` header convention). When the ClickHouse usage sink is rebuilt,
it lands as a `lifecycle` observer behind `app/usagelog`'s `Sink`
interface — the deleted `pkg/eventlog/clickhouse.go` insert logic is
recoverable from git history as a reference.

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
- Higher-fidelity smoke: `make smoke-mock` replays recorded OpenAI
  fixtures (`wyolet/spec-mock-openai`) through relay → Caddy →
  spec-mock-openai. Validates wire-level dispatch end-to-end with real
  recorded conversations including parallel tool-calls + streaming.
- When a misconfigured upstream trips key-pool breakers, `make
  breakers-reset` clears `secret_health:*` keys in valkey so subsequent
  fixed requests can land instead of returning "no healthy keys in
  pool".

## When in doubt

- Don't over-engineer. Push back when a feature isn't earning its
  complexity.
- Boring choices on the edges, smart choices in the middle. The router
  (chi) is the edge; the pipeline is the middle.
- "Three similar lines is better than a premature abstraction."
- Read the design docs under `docs/` (`docs/canonical-protocol.md` and
  the per-subsystem guides) before proposing architecture changes.
- The roadmap (`docs/roadmap.md`) names every known follow-up — check
  it before opening a new design question.
