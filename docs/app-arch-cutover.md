# `app/` architecture cutover — handoff

This branch (`feat/new-arch`) is at a clean checkpoint. The new catalog
plane is feature-complete, tested, and pushed. The legacy
`internal/catalog` still serves production paths; both arches coexist.
The remaining work is the **cutover** — wiring the request and admin
paths to the new arch and deleting the old.

This doc captures: what's done, what's deferred and why, decisions
locked, and the staged plan to finish.

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

### Stage 1 — `cmd/relay/main.go` boot swap

Replace `internal/catalog.PGStore` construction with
`app/catalog.Bootstrap(ctx, BootstrapOptions{...})`. Start
`go listener.Run(ctx)`. Keep old `internal/catalog` alive in parallel
(legacy handlers still call into it). No handler changes yet — the new
catalog runs hot but unused.

Validation: existing tests stay green, `make test-integration` passes,
manual smoke against `make dev`.

### Stage 2 — OpenAPI reorganization

Split `cmd/relay/openapi.go` (902 lines) into focused files:
- `http_inference.go` — `/v1/*` + `/healthz`
- `http_auth.go` — `/auth/login`, `/auth/logout`, `/auth/whoami`
- `http_crud.go` — generic CRUD wiring across all kinds
- `http_misc.go` — `/version`, `/reload`, `/master-key/generate`
- `openapi.go` — Huma API setup + router mount

URL changes (control plane drops the `/control/` prefix entirely — it's
served on its own listener):
- `POST /auth/login`, `POST /auth/logout`, `GET /auth/whoami`
  *(grouped)*
- `GET /version`, `POST /reload`, `POST /master-key/generate`
- CRUD (id-routed, uniform):
  - `/providers`, `/hosts`, `/models`, `/policies`, `/pricings`,
    `/rate-limits`, `/host-keys`, `/relay-keys`
- **Renames:** `ratelimits` → `rate-limits`, `keys` → `relay-keys`,
  `secrets` → `host-keys`
- **Dropped:** `/routes`, `/attachments`, `/passthrough`, the bespoke
  `/secrets/{name}` handlers
- **New:** `/hosts`, `/pricings`

### Stage 3 — CRUD factory rewire

`pkg/admin/crud` generic factory currently uses `internal/catalog.PGStore`
methods. Switch to a narrow interface satisfied by every `app/X.Store`:
```go
type Store[T any] interface {
    List(ctx) ([]*T, error)
    Get(ctx, id) (*T, error)
    Upsert(ctx, *T) error
    Delete(ctx, id) error
}
```
Admin writes go to `stores.X.Upsert/Delete`; NOTIFY auto-propagates to
the snapshot. Drop direct snapshot manipulation from admin handlers.

### Stage 4 — Hot path rewire

`internal/pipeline` and `pkg/transport` deep-coupled to
`internal/catalog.Secret`, `.Provider`, `.Policy`, `.Model`,
`.ResolvedRule`. Translate to `app/hostkey.HostKey`, `app/provider.*`,
etc. Most accessor names line up; some fields differ (e.g.
`Secret.ValueFrom` → `HostKey.Spec.ValueFrom`).

Tests: `internal/pipeline/pipeline_test.go` (1310 lines) will need
rewiring — likely worth a focused subagent pass.

### Stage 5 — Delete legacy

Remove `internal/catalog/`, `internal/import/litellm/`,
`cmd/relay/import.go`. Update `cmd/relay/seed.go` to be a thin wrapper
around `app/seed.Run`. Drop `internal/configstore` if it was
catalog-coupled.

### Stage 6 — E2E integration

Extend `deploy/compose/docker-compose.test.yml` with `relay-a` service.
New `integration/e2e_test.go` (build tag `integration`):
1. Boot compose stack
2. Wait for relay-a healthz
3. POST a Provider, Host, Model, HostKey, Policy, RelayKey via the
   admin HTTP API
4. Sleep ~1.5s for NOTIFY propagation
5. POST `/v1/chat/completions` with the relay key + a request that
   targets the new model
6. Assert: 200, expected upstream selected, response body matches
   mock-upstream's reply

Mock upstream lives in the same compose file as a tiny Go process or
httptest binary. The point is **catching real bugs unit tests don't see**
— per-shape transforms, header forwarding, key-pool selection, NOTIFY
end-to-end.

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
