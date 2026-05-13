# `app/` architecture cutover — handoff

This doc tracks the cutover from the legacy `internal/catalog`-based
request path to the new `app/`-based one. It started as a handoff
checkpoint when the new catalog plane was feature-complete on the
`feat/new-arch` branch and now also records what's shipped vs pending
as stages land.

## Status (as of stage 4 open PR)

| Stage | Title | Status | PR |
|---|---|---|---|
| 1 | `cmd/relay/main.go` boot swap | Shipped | [#111](https://github.com/wyolet/relay/pull/111) |
| 2 | `app/httpapi/` scaffold (replaces stage-2 in-package split) | Shipped | [#111](https://github.com/wyolet/relay/pull/111) |
| 3 | Admin CRUD + `/auth/*` + sessions | Shipped | [#112](https://github.com/wyolet/relay/pull/112) |
| 4 | Hot path: `app/pipeline` + `app/api/*` + inference handlers | Open | [#113](https://github.com/wyolet/relay/pull/113) |
| 5 | Delete legacy | Pending | — |
| 6 | E2E integration test against compose with mock upstream | Pending | — |

The legacy tree (`internal/pipeline`, `internal/routing`, `internal/api/*`,
`internal/keypool`, `cmd/relay/_legacy/`) is no longer wired into the
boot path from stage 4 onward but stays in tree until stage 5.

---

## 1. What's done

### Domain layer — eight typed entities under `app/`

| Package | Role |
| --- | --- |
| `app/provider` | Vendor identity (display fields only) |
| `app/host` | Serving endpoint — `BaseURL`, region/backend bits |
| `app/model` | Model spec; `Spec.Hosts []HostBinding` |
| `app/hostkey` | Upstream auth credential, env-ref or stored-mode AES-GCM |
| `app/ratelimit` | Rule set with meters/strategies |
| `app/policy` | Hub: `ModelIDs`, `HostKeyIDs`, `RateLimitID`, `KeySelection` |
| `app/relaykey` | Inbound customer API key → policy |
| `app/pricing` | Rate sheet, owner=Host, applied to N Models, with `AboveTokens` tier |

Every entity has `pkg/meta.Validator`-backed `Validate()`, JSONB
marshal/unmarshal, and a `Store{List, Get, Upsert, Delete}` against
sqlc-generated queries.

### Composition layer — `app/catalog`

- **`Snapshot`** — immutable read-side. Holds every enabled row of every
  kind in byID/byName indices, plus reverse joins (`modelsByPolicy`,
  `pricingByModelHost`) and a reverse-ref index (`refsBy*`) used by
  cascade invalidation.
- **`Reload(ctx)`** — full rebuild from PG. Used at boot and as the
  fallback path.
- **`Apply{Kind}{Upsert,Delete}`** — COW reconciler. Each method clones
  the snapshot, mutates the clone, atomic-swaps. Cascade-invalidation
  walks `Dependents()` and evicts dependents whose cross-refs became
  dangling (worklist-based, no recursion).
- **`Listener`** — `LISTEN catalog_events` on a dedicated pool
  connection. 1-second debouncer coalesces by `(kind, id)` with
  last-op-wins. On flush, drained events are **sorted by kind in
  dependency order** (provider → host → ratelimit → model → hostkey →
  policy → pricing → relaykey) before applying.
- **`Bootstrap(ctx, opts)`** — single-call factory: wires all eight
  stores, optionally auto-seeds from YAML when PG is empty, runs the
  initial `Reload`, returns a primed `Listener` + `Stores` bundle.

### Persistence

- Migration 0009: `hosts` table + `policy_models` / `policy_host_keys`
  junctions + `policies.rate_limit_id` column
- Migration 0010: `pricings` + `pricing_models` junction
- Migration 0011: `catalog_notify()` and `catalog_notify_junction()`
  trigger functions; AFTER INSERT/UPDATE/DELETE triggers on every
  catalog table emitting `kind:op:id` on channel `catalog_events`
- sqlc queries for the above; `Get-by-id` query per entity (input to
  the listener fetch path)

### Manifest / seed / import

- `app/manifest` — name-based YAML DTOs for the eight kinds, parser
  (multi-doc), translate ↔ domain, Resolver/ReverseResolver
- `app/seed.Run` — orchestration: parse → list existing PG state → mint
  UUIDs for new names → upsert in dependency order
- `cmd/litellm-import` — fetches LiteLLM JSON, emits manifest-shape
  YAMLs (Provider, Host, Model with `spec.hosts[]`, Pricing with tier
  rates from `*_above_*_tokens` fields)

### Tests

- Unit tests for every domain `Validate()`
- `app/catalog/refs_test.go` — three invariants on the reverse-ref index
- `app/catalog/reconcile_test.go` — 7 scenarios (upsert/replace/delete
  cascades to Pricing+Policy, disable==delete, toggle idempotent, ref
  invariants)
- `app/catalog/property_test.go` — fuzz: 4 seeds × 500 random events
  each; after every event re-asserts every snapshot invariant under
  `-race`
- `app/catalog/notify_test.go` — payload parser + debouncer
- `app/catalog/integration_test.go` (build tag `integration`) — full
  end-to-end against an ephemeral Postgres via
  `deploy/compose/docker-compose.test.yml`:
  - bootstrap over empty DB
  - direct `stores.Provider.Upsert` → NOTIFY → snapshot reflected
  - model disable cascades to its Pricing via reverse-ref
  - HostKey stored mode roundtrips through encrypt/decrypt + DB
- `make test-integration` brings the pg up, runs everything with
  `-race`, tears down on exit (zero or non-zero)

### Regenerated config

`config/` was nuked and regenerated via `cmd/litellm-import` — 3 hosts,
3 providers, 132 models, 110 pricings with tier rates. `ratelimits/` and
`users/` left untouched (hand-authored).

---

## 2. Locked decisions

These are deliberate, debated, and should not be revisited without good
reason.

### Snapshot inclusion rule: **enabled-only**

Every enabled row of every kind enters the snapshot. No reachability
filter. Disabling a Policy no longer evicts its referenced Models /
HostKeys / RateLimits — they're independently enabled. The "enabled"
flag is the entire toggle mechanism.

### Provider vs Host

Provider = vendor identity (display fields only — no baseURL). Host =
serving endpoint (baseURL, region, backend hints). One vendor can have
multiple hosts (Anthropic direct vs Bedrock vs Vertex). A Model's
`spec.hosts[]` lists the bindings; `Pricing.Owner.Kind == OwnerHost`
because rates differ per host even for the same Model.

### Pricing shape

```go
type Spec struct {
    Currency       string
    TargetModelIDs []string      // many Models per tier
    Rates          []Rate
    Enabled        *bool
}
type Rate struct {
    Meter       Meter   // tokens.input, tokens.output, tokens.cache_read,
                       //   tokens.cache_write, tokens.reasoning
    Unit        Unit    // per_million | per_unit
    Amount      float64
    AboveTokens int     // 0 = base tier; >0 = context-tier cliff
}
```

Anthropic 200k, Gemini 128k, OpenAI 272k all do context-tier cliffs —
real pricing, ~70 of the ~thousands of models. `AboveTokens` covers all
of them. `RateFor(meter, tokens)` picks the largest qualifying
threshold. Mode tiers (batch/priority/flex) deferred — see §3.

Pricing meters intentionally narrowed to OpenRouter's 8-key list +
reasoning (5 token meters in v1). Audio/image/web_search/etc. defer
until we actually route those request types.

### NOTIFY / reload

- One code path: every admin write goes through PG. The trigger emits
  NOTIFY. The listener (every pod, including the writer) consumes,
  fetches the affected row by id, and applies via the reconciler.
- 1-second debounce window. Bulk-admin coalesces; per-write latency
  before snapshot reflects ≈ 1s + epsilon (acceptable for control
  plane).
- Apply-order sort by kind dependency (parent kinds first). Caught a
  real bug via integration test — without the sort, a 4-row bulk
  insert lands children before parents and cross-ref validation
  silently drops them, leaving the pod in a stale state until restart.

### Reverse-ref index, no GC

Snapshot maintains `refsBy{Provider,Host,Model,HostKey,RateLimit,Policy}
map[parentID]refSet`. Each row registers its outbound refs at insert,
unregisters on remove. Reconciler delete walks `Dependents()` and
cascade-evicts dependents whose cross-refs become dangling. Bounded
fan-out, no mark-sweep, no dependency-graph traversal.

### CRUD identity model

Each row carries:
- `metadata.id` — UUIDv7, immutable, server-stamped on create
- `metadata.name` — DNS-1123 slug, mutable, unique per kind
- `metadata.displayName` — free text, no ref impact

Cross-refs in spec fields use id, not slug (migration 0009 change).
Manifest DTOs use names externally; the resolver translates on
parse/render.

### Token counting

We trust upstream. Provider response carries `usage.{input_tokens,
output_tokens, cache_read_input_tokens, ...}`; per-shape parsers in
`pkg/api/*` extract them into a universal `pkg/usage.Tokens` map.
Self-tokenizing would be 5–10% off and pull in a model→tokenizer map
we'd own.

---

## 3. Deferred

Each of these was discussed and explicitly punted. Capturing the
reasoning so we don't re-litigate without new information.

### Route entity

The customer-facing "unit of intent" (`X-Relay-Route: prod-cheap`)
bundles policy + ACL + budget + storage. Listed in `CLAUDE.md` as the
spine of the domain model but skipped this round — we have no concrete
need yet beyond what Policy already gives. **Likely the first thing to
revisit when the hot path enforcement work bites.**

### PassthroughConfig

Singleton row that lists routes/methods bypassing typed-shape
transforms. Not needed until we re-enable passthrough on the inbound
side; the data has no consumers in the new arch.

### Org / Project / Team / Budget

Per CLAUDE.md the full hierarchy is `Org → Project → Route → ApiKey →
KeyPool → ProviderKey → Budget` plus RBAC. We have Policy and RelayKey
only. Wedge says deferred indefinitely until we have multi-tenant
traffic.

### ClickHouse / eventlog persistence

`pkg/eventlog` and `internal/usage` stay in tree but the ClickHouse
backend is not on a critical path. We will come back to logs + usage
when that becomes the work. The integration-test build error was fixed
so `make test-integration` runs clean.

### Mode-tier pricing (batch / priority / flex)

LiteLLM exposes `_batches`, `_priority`, `_flex` variants per meter.
These are separate price sheets, not request-shape conditions. When we
need them, the shape is a second `Pricing` row with
`metadata.labels.tier: batch` and a request-side picker that reads a
header. No schema change required.

### Pricing meters beyond the v1 five

Audio (per-second), images (per-image), video, web_search,
code_interpreter, file_search, character/pixel/query — all real and
all in LiteLLM. Deferred to a follow-up when we actually proxy those
request types. The `Rates []Rate` shape grows by adding rows, not by
schema change.

### Pre-write patch validation

`internal/configstore` had a `ValidateWithPatch` helper that ran a
proposed mutation against a snapshot before commit. Not ported — the
new arch validates on every reload anyway, and admin writes are
single-row.

### sqlc layout cleanup

`internal/storage/gen/queries.sql` is hand-authored despite living
under `gen/`. Confusing but historical. Worth moving to
`internal/storage/queries/` someday; not in scope now.

---

## 4. Cutover plan — six stages

Each stage is its own focused PR with green tests before merging.

### Stage 1 — `cmd/relay/main.go` boot swap *(shipped — PR #111)*

`main.go` now boots `app/catalog.Bootstrap` exclusively. Legacy
`cmd/relay/*.go` files moved aside under `cmd/relay/_legacy/` (the `_`
prefix makes Go skip the dir entirely — files stay as porting
reference without compiling). Auto-seed via
`BootstrapOptions.AutoSeedDir` when `RELAY_AUTO_SEED_IF_EMPTY=1`.
NOTIFY listener runs in a goroutine. Added `Storage.Pool()` accessor
on `internal/storage` so the composition root can hand the pool to
Bootstrap. `app/manifest` skips foreign kinds (`User`/`Group`/`Role`)
so the catalog seeder coexists with `config/users/` identity YAML.

### Stage 2 — `app/httpapi/` scaffold *(shipped — PR #111, alongside stage 1)*

Original plan was to split `openapi.go` into in-package files. We went
further and lifted the whole HTTP layer into its own package:

```
app/httpapi/
  httpapi.go       — shared OpenAI-shape error envelope, version const,
                     per-plane schema namer (avoids provider.Spec vs
                     host.Spec collisions in the OpenAPI registry)
  middleware.go    — HumaAuth (net/http MW → huma per-op MW)
  inference/       — data plane (Mount + huma.API + Deps)
  control/         — admin plane (Mount + huma.API + Deps)
```

Each plane: `Deps` + `Mount(chi.Router, Deps) huma.API`. `cmd/relay/main.go`
mounts both planes on separate listeners (`RELAY_PORT`,
`RELAY_CONTROL_PORT`).

### Stage 3 — Admin CRUD + `/auth/*` *(shipped — PR #112)*

The control plane is now a real admin API serving auth + CRUD against
`app/catalog`:

- `/auth/login`, `/auth/logout`, `/auth/whoami` — bcrypt-aware password
  verification via `internal/identity.Verify`; opaque server-side
  sessions via `alexedwards/scs/v2` with a kv-backed store
  (`app/session`).
- 8 catalog kinds × 5 routes each (`/providers`, `/hosts`, `/models`,
  `/host-keys`, `/rate-limits`, `/policies`, `/pricings`, `/relay-keys`):
  - `GET /{plural}`, `GET /{plural}/{ref}` (slug or id),
    `POST /{plural}` (server stamps id+slug),
    `PUT /{plural}/by-id/{id}`, `DELETE /{plural}/by-id/{id}`.
- `/master-key/generate`, `/reload`, `/version`.
- 30 paths in `/openapi.json`.

**Future-proof seams baked in** for the multi-tenant / IAM work later:

- `app/actor` — `Actor{UserID, Username, SessionID, AdminToken}` in
  `context.Context` via typed key. Reserved `ActiveOrgID`/`Roles` slots
  for the org-scoped permissions work.
- `app/authz` — `Authorizer` interface with `Authorize(ctx, action,
  resource)`. v1 impl `AlwaysAllowAuthenticated` grants any action to
  any authenticated caller; swap impl when org-scoped permissions land,
  no handler changes.
- Identity stays in `internal/identity` for v1 — YAML-backed; no `users`
  table. Migrating to Postgres-backed users is its own slice when SaaS
  signup lands (likely alongside Org/Workspace).

Two explicit non-goals confirmed during stage 3:

- `/passthrough`, `/attachments` dropped from routes permanently —
  replaced by a future `/settings/*` typed sectioned API + KV
  `settings(key TEXT, value JSONB)` table (NOT a one-row table per
  section).
- Per-user JWT for programmatic API access is **complementary**, not a
  replacement. Slots in cleanly later as a third caller type alongside
  cookies and relay keys.

### Stage 4 — Hot path rewire *(shipped — PR #113)*

The data plane now serves `/v1/chat/completions`, `/v1/messages`,
`/v1/models`, `/healthz` against `app/catalog`.

Layout (LOC approximate):

| Package | Lines | Role |
|---|---|---|
| `app/adapter` | ~40 | `Kind` enum (`openai`, `anthropic`). Lives on `model.HostBinding`, not Host — Bedrock serves Claude (Anthropic) and Llama (OpenAI) from one endpoint. |
| `app/pipeline` | ~280 | **Pure orchestration**: reserve → pick key → `Adapter.Call` → stream → post-flight commit + RecordSuccess in a detached goroutine. Knows nothing about catalog snapshots, JSON shapes, or HTTP routing. |
| `app/keypool` | ~290 | Port of `internal/keypool` to `*hostkey.HostKey` + `*policy.Policy`. Pick/Record* signatures dropped the unused Provider/Model args. |
| `app/routing` | ~200 | snapshot lookup → `Plan{Model, Policy, HostBinding, Host, Keys, Rules}`. |
| `app/ratelimit` | ~50 | `Resolve(policy, rl) → []pkgratelimit.Rule`. Pipeline calls `pkg/ratelimit` directly; no adapter-wrapper layer. |
| `app/api/openai` | ~250 | `pipeline.Adapter` impl, Bearer auth. |
| `app/api/anthropic` | ~260 | `pipeline.Adapter` impl, `x-api-key` auth, `anthropic-version` default, passthrough headers, 529 → FailureServerError. |
| `app/httpapi/inference` | ~400 | Huma ops + relay-key Bearer middleware. |

**Key design decisions made during stage 4** (push back on the original
1:1 port plan was correct):

- **Pipeline is pure orchestration.** Removed the legacy pipeline's
  passenger concerns: `usage.Lifecycle` stamping, token extraction
  (delegated to Adapter), 429 envelope shaping (handler), anonymous-
  mode synthetic fallback (handler), cross-shape transform branching
  (dropped), channel-based transport messages (dropped), `Outbound` vs
  `DoUpstream` branching (unified into Adapter.Call).
- **Cross-shape translation dropped for v1.** Hit `/v1/chat/completions`
  for a model whose binding declares `adapter: anthropic` → 400. Same
  the other way. CLAUDE.md: "same-format passthrough is the 95% case."
- **Anonymous traffic is a separate package**, not a branch in pipeline.
  Future `app/proxy` (or similar) when passthrough comes back.
- **Async post-flight stays** — never blocks the response (per
  CLAUDE.md hot-path rule).
- **`HostBinding.Adapter` is required**. `cmd/litellm-import` emits it
  per provider (openai/ollama → `openai`, anthropic → `anthropic`);
  132 model YAMLs regenerated.

Full upstream call exercise is stage 6 (compose E2E with mock upstream
service).

### Stage 5 — Delete legacy

Now that stages 1–4 ship, all of these are dead code:

- `internal/pipeline/`, `internal/routing/`, `internal/api/*`,
  `internal/keypool/`, `internal/ratelimit/adapter.go`,
  `internal/provider/*` (legacy outbound clients — `app/api/*` issues
  its own HTTP).
- `internal/catalog/`, `internal/import/litellm/`, `internal/configstore/`.
- `cmd/relay/_legacy/` — porting reference, can go once stages 1–4 are
  merged and we don't need it for cross-checks.
- `pkg/admin/crud/` — superseded by the generic CRUD in
  `app/httpapi/control/crud.go`.

Order matters: do `internal/*` first (independent), then
`cmd/relay/_legacy/`, then `pkg/admin/crud/` (independent of the
internal tree but worth confirming nothing imports it). One PR is fine
— it's pure deletion with no design decisions.

### Stage 6 — E2E integration

Extend `deploy/compose/docker-compose.test.yml` with a `relay` service
and a `mock-upstream` service. New `integration/e2e_test.go` (build
tag `integration`):

1. Boot compose stack (pg + relay + mock-upstream).
2. Wait for relay `/healthz`.
3. Use the admin Bearer to:
   - `POST /hosts` pointing at the mock upstream
   - `POST /host-keys` with a test key value
   - `POST /policies` linking a seeded model to the host-key
   - `POST /relay-keys` for the policy
4. Sleep ~1.5s for NOTIFY propagation (or poll the snapshot endpoint
   if we add one).
5. `POST /v1/chat/completions` with the relay key.
6. Assert: 200, correct upstream URL hit, response matches mock's reply,
   `RecordSuccess` fired (verify via Selector state in kv).

Mock upstream lives in the compose file as a tiny Go binary or a
prepopulated `httptest`-like service. The point is **catching real
bugs unit tests don't see** — per-shape transforms, header forwarding,
key-pool selection, NOTIFY end-to-end, adapter dispatch on
`HostBinding.Adapter`.

---

## 5. Branch state

- Branch: `feat/new-arch`
- Tip: see `git log --oneline main..HEAD`
- Tests: `go test ./...` and `make test-integration` both green
- Compose: `deploy/compose/docker-compose.test.yml` brings up
  ephemeral pg on `127.0.0.1:5499`

## 6. Memory references

Auto-memory entries (in
`~/.claude/projects/-Users-abror-projects-wyolet-relay/memory/`) the
next session should consult:

- **`feedback_git_workflow`** — branch + PR discipline; ask before
  large commits
- **`feedback_subagent_model`** — pass `model: "sonnet"` for grunt-work
  subagents; opus only for orchestration
- **`feedback_storage_boundary`** — pgx/SQL stays inside
  `internal/storage`. `app/X.Store` already respects this; the cutover
  must too
- **`project_canonical_shape`** — OpenAI is the canonical hub; cross-
  shape transform pairwise via OpenAI. Lossiness deferred until OpenAI
  bites real traffic
- **`project_followups_open`** — superseded by this doc; some entries
  (stale `kind: Pool`, ui-fetch, secrets handler still slug-routed)
  are resolved or about to be by the cutover
- **`reference_dev_stack`** — gpu Fedora hosts shared infra; relay
  data plane public URL `relay-api.wyolet.dev`, control
  `relay-control-api.wyolet.dev`, UI `relay.wyolet.dev`
- **`reference_port_catalog`** — 100-port blocks per project; Mac runs
  dev, Fedora runs infra

## 7. Useful commands

```bash
# Run all unit tests
go test ./...

# Integration tests (spins up ephemeral pg, tears down)
make test-integration

# Regenerate YAMLs from LiteLLM
go run ./cmd/litellm-import --out config

# Rebuild the dev stack
make dev

# Seed against dev pg (legacy path still works)
make seed
```

## 8. First moves for the next session

1. Read this doc end to end.
2. Skim `app/catalog/{snapshot.go, build.go, reconcile.go, notify.go,
   boot.go}` — the spine of the new arch.
3. Skim `app/catalog/integration_test.go` — shows how the pieces glue
   in practice.
4. Start **Stage 1** (main.go boot swap). Keep `internal/catalog`
   running in parallel until Stage 4 lands.

Don't try to do stages 1–4 in one PR. Each is its own diff and review.
