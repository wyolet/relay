# List filtering — design + follow-ups

The admin/control list endpoints filter, sort, and window server-side via
a single declarative engine. This doc records what shipped and the
remaining per-resource work, split into independently-shippable tasks.

## Shipped

- **`pkg/usage` relocation** (#262) — moved the usage `Event` + filter/
  aggregate engine out of the vendorable SDK into the server module, so
  config-list and usage filtering can share one contract without crossing
  the module boundary. `sdk/usage` keeps only the pure wire shapes
  (`Tokens`, `UpstreamTiming`, `ReasoningTiming`).
- **`pkg/filter` engine** (#263) — generic, allowlist-based query engine.
  A resource declares a `Schema[T]` of typed `Field` accessors; the engine
  parses `url.Values` → validated filter/sort/window → applies to the
  in-memory `[]*T`. Contract: equality · repeatable ⇒ IN (OR within a
  field) · bool · `_min`/`_max` & `_from`/`_to` ranges · free-text `q` ·
  `sort` (`-`=desc) · `limit`/`offset` · **400 on unknown/malformed key**.
  Wired into `registerKind` behind an optional `filterSchema` via the
  `withRawQuery` middleware; list responses carry a pre-window `total`.
- **Metadata timestamps** (#264) — `created_at`/`updated_at` surfaced on
  `meta.Metadata` (read-only, from the dedicated columns, not JSONB).
  Adds `created_from/to`, `updated_from/to`, `sort=created/-updated` to
  every wired schema.

Baseline schemas live in `app/httpapi/control/list_schemas.go` for
policies, models, hosts, relay-keys.

## Design invariants (keep these when extending)

- **Two executors, one contract.** Config lists filter a materialised
  slice (`pkg/filter`, in-memory). The usage/event path keeps its own
  store-aware `pkg/usage.EventQuery` because it pushes filters into
  ClickHouse SQL. Don't try to unify the executors — unify only the wire
  contract (param spellings/semantics).
- **Typed accessors, single source of truth.** Each `Field` is a typed
  closure over `T`; renaming the underlying spec field is a compile error
  in the schema. The param name, allowlist entry, doc, and match logic all
  derive from one `Field` literal. No second list to update.
- **No silent widening.** Any query key not on the allowlist → 400.

---

## Follow-up tasks (each its own PR)

### F1. Model capability filter — `?capability=` (standalone; do first)

**What.** Filter models by the `model.Capabilities` bitset (`chat`,
`embeddings`, `streaming`, `tools`, `parallelTools`, `vision`, `audio`,
`promptCache`, `reasoning`, `jsonMode`, `structuredOutputs`, `batch`).
Highest-value catalog filter — "show me vision + tools models."

**Why it's separate.** It doesn't fit the default repeatable-field
contract cleanly:

- The default `Repeat` semantics are **OR within a field** (`?model_id=a&
  model_id=b` ⇒ a OR b). But the natural intent of `?capability=vision&
  capability=tools` is **AND** — "models that support *both*." Shipping it
  as plain OR would surprise users and the UI.
- A model's "value" for this field isn't one string — it's the *set* of
  capability names whose bool is true. Membership, with AND across the
  requested values.

**Decision needed before building.** Pick one:
  1. Add a per-`Field` `MatchAll bool` to `pkg/filter` — when set, a
     repeatable membership field requires the item's set to contain *all*
     requested values (AND) instead of any (OR). `capability` sets it.
     Cleanest; small engine change; reusable (e.g. `tag` AND-mode later).
  2. Bespoke predicate outside the generic engine (a custom field kind or
     a hand-written matcher). Avoids touching the engine but breaks the
     "everything is a declared Field" property.

Recommendation: **option 1** — one bool on `Field`, documented as
"repeatable membership, all-of." Validate the requested values against the
known capability-name set (enum) so typos 400.

**Where.** `pkg/filter` (the `MatchAll` flag + match logic) +
`modelFilter` in `list_schemas.go` (a `GetMulti` returning the model's
enabled-capability names + the capability-name enum). Size: S.

### F2. Remaining per-resource allowlist expansion

- **Models:** `provider_id` (via `metadata.owner`), `version`,
  `max_output_tokens` (shipped), `modality`, `released_from/to` +
  `deprecated_from/to` (from `spec.releaseDate`/`deprecationDate`).
- **Host-keys:** `value_kind=env|stored` (audit: env-ref vs
  AES-GCM-in-PG), plus the baseline `enabled`/`host_id`/`policy_id`/
  `default_tier` schema (host-keys aren't wired yet).
- **Providers / pricing:** whole schemas (not wired). Providers:
  `enabled` + `q`. Pricing: `target_model_id`, `currency`, `meter`,
  `unit`, `enabled`, `has_tiers` (`AboveTokens>0`).
- **Uniform `label=k=v`** across all kinds (every entity has
  `metadata.Labels`) — best added as a first-class helper in `pkg/filter`
  rather than per-resource. Size: M.

### F3. Host-key circuit-breaker state filter — `?health=open|closed|half_open`

Filter the host-keys list by live breaker state, reusing the
`Selector.ReadCircuit` work (#256). **Heavier:** it's a snapshot + kv
join, not a pure store-slice filter — the generic `pkg/filter` Apply over
`store.List` can't see breaker state. Needs either an enrich step that
stamps state onto the row before filtering, or a dedicated code path.
Design before building. Size: M–L.

### F4. Logs/Usage filter gap-fill

Additive fields on `usageFilterInput` → `pkg/usage.EventQuery` →
`matches()` + the file/ClickHouse/postgres/valkey readers (and SQL
`buildWhere`). Frontend-requested + proposed:

- `host_key_id` (filter exists for group_by, not as a filter — asymmetry)
- `status` exact (repeatable) + `status_class=2xx|4xx|5xx`
- `error` bool · `streamed` bool · `has_payload` bool · `has_reasoning`
- `requested_model` (repeatable) · `attempts_min`
- `duration_ms_min/max` · `ttft_ms_min/max` (from `Upstream.ResponseStart`)
- `tokens_total_min/max` (and input/output) · `extras.<k>=v`
- `q` free-text · `sort` (`ts`/`duration_ms`/`status`)

Arguably the highest UI impact (kills the most mock data). Size: M.
