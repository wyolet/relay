# List filtering

The control-plane list endpoints and the logs/usage read endpoints filter,
sort, and window server-side via one declarative engine (`pkg/filter`) and a
shared wire contract.

## Wire contract

Plain query params, no DSL:

- equality `field=value` · repeatable key ⇒ IN (OR within a field)
- bool `field=true|false`
- numeric range `field_min` / `field_max`
- time range `field_from` / `field_to` (RFC3339)
- free-text `q` · label selector `label=k=v` (repeatable, AND)
- `sort=field` (`-` prefix = desc) · `limit` / `offset`
- unknown or malformed key → **400** (no silent widening)

Filters compose with AND; repeated same-key values are OR within that field.
List responses carry a pre-window `total`.

## Design invariants

- **Two executors, one contract.** Config lists filter a materialised slice
  in memory (`pkg/filter` over `store.List`). The usage/event path keeps its
  own store-aware `pkg/usage.EventQuery` — it pushes filters into ClickHouse/
  Postgres SQL (`buildWhere`) and an in-memory `matches()` for the file/valkey
  scan backends. Don't unify the executors; only the wire contract is shared.
- **Typed accessors, single source of truth.** Each `filter.Field` is a typed
  closure over `T`. Renaming the underlying spec field is a compile error in
  the schema — the param name, allowlist entry, OpenAPI doc, and match logic
  all derive from one `Field` literal. Config schemas: `app/httpapi/control/
  list_schemas.go`. Repeatable membership with AND semantics (e.g.
  `?capability=`) uses `Field.MatchAll`; `?label=` uses `Schema.Labels`.

## OpenAPI is the codegen contract

The frontend generates its TS client from `/openapi.json`, so every filter
param MUST appear there:

- **Config lists:** params are read from raw `url.Values` (huma binds static
  structs, but the allowlist is per-resource dynamic) via the `withRawQuery`
  middleware, and surfaced in the spec by setting `Operation.Parameters` from
  `filter.Schema.Params()`.
- **Logs/usage:** typed `query:"..."` fields on a shared input struct.

Two huma footguns here (both load-bearing):

1. **huma silently skips UNEXPORTED struct fields** (`if !f.IsExported()`).
   The shared usage input type and the field that embeds it must be exported
   (`UsageFilterInput`) — otherwise huma never recurses into the embed and the
   entire filter surface vanishes from both the spec AND runtime binding, with
   no error.
2. **huma rejects pointer query params.** For tri-state (unset vs false) use a
   string enum (`true`/`false`/`""`) parsed to `*bool` at the boundary.

## Remaining work

Tracked in `roadmap-oss.md` (A21): host-key breaker-state filter (`?health=`,
needs a snapshot+kv join) and the logs/usage tail (`sort`, `tokens_total`,
`has_reasoning`/`has_payload`, `extras`).
