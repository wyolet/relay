# SDK initiative — overview (EPHEMERAL)

> **Lifecycle:** these `docs/sdk-spec/*` files are throwaway implementation
> specs. They guide the multi-PR build, then get distilled into permanent docs
> (`docs/canonical-protocol.md` for the module boundary, a new `docs/sdk.md` for
> the public client) and **deleted**. Do not link to them from permanent docs.

## Goal

Turn `pkg/relay/client` into a **clean, vendorable public Go SDK** that:

1. Lets a consumer pull **only the client + pure translation library** as a Go
   module — never the relay server source, never pgx/redis/clickhouse.
2. Lets a consumer **call any catalogued upstream host directly** by a model
   reference (`gpt-4o`, `openai/gpt-4o`, `gpt-4o@bedrock`) with zero manual
   baseURL/path/auth wiring — the curated catalog supplies it.
3. **Calculates cost** from the response's `usage.Tokens` using the host's
   pricing rate sheet, for hosts that carry pricing (optional per host).

Relay itself keeps using the canonical shape + adapters; only the `client`
(and the new `catalog` pkg) are SDK-exclusive.

## Architecture decisions (locked)

- **Two-module monorepo.** New module `github.com/wyolet/relay/sdk` holds the
  vendorable library; the existing `github.com/wyolet/relay` (server) depends on
  it. `go.work` ties them for dev. Dependency direction is **server → sdk,
  never reverse** — this is just the existing "canonical knows nothing"
  rule (codebase rules 1 & 10) formalized as a module boundary.
- **The `sdk` module contains exactly the pure closure:**
  `v1/`, `adapters/{openai,anthropic,gemini}/`, `usage/`, `catalog/`, `client/`.
  Nothing else (notably NOT `pkg/kv`, which pulls redis).
- **Catalog reaches the pure SDK via a generated data file, not an import.**
  `cmd/catalog-embed` (server module — may import `app/`) reads the catalog and
  writes `sdk/catalog/catalog.json`; `sdk/catalog` `//go:embed`s it. No import
  edge crosses the boundary, so rules 1/2/4/10 greps keep passing.
- **`Adapter` = the atomic wire bundle** `{translator, path, auth}`, defined in
  SDK code, keyed by adapter name (`openai|anthropic|gemini`). The catalog only
  stores the adapter *name*; code owns *how* to call. (Renamed from the
  "Shape" working term — `adapter` matches the catalog field and relay's
  `adapter.Spec`.)
- **`Target` = `{baseURL, Adapter, upstreamModel, pricing[]}`** — immutable,
  produced by resolving a catalog binding or by explicit manual construction.
  Overrides swap whole consistent pieces (whole Adapter, or baseURL); there is
  no half-mutated intermediate state, so you cannot pair (e.g.) the anthropic
  translator with an OpenAI path.
- **Resolution unit is the binding**, addressed by model ref, mirroring relay's
  alias index (bare / `provider/model` / `model@host`). Host-only is
  underspecified (pricing is per model-on-host, adapter is per-binding, upstream
  name is per (model,host) snapshot).
- **Catalog↔code drift is a generation-time failure.** `cmd/catalog-embed`
  asserts every adapter name in the catalog has a registered SDK `Adapter`;
  unknown adapter ⇒ `make catalog-embed` fails, never production.
- **Drafts never reach the embed** — the generator reuses the seeder's
  draft-skip (`drafts/` dirs + `*.draft.yaml`), so the SDK only ever ships
  non-drafted catalog items, exactly like the relay seeder.

## Non-goals

- No separate repo (monorepo only) — keeps the generator next to its output.
- No new third-party deps in `sdk` (only `coder/websocket`, already present).
- No agent/tool-call loop, no retry framework — out of scope for this initiative.
- No provider-level info in the client — host + binding + pricing only.

## Stages (one stage = one PR)

| Stage | PR | Spec |
|---|---|---|
| 1 | Module split (mechanical, zero behavior change) | `01-module-split.md` |
| 2 | Catalog embed pipeline (`sdk/catalog` + `cmd/catalog-embed`) | `02-catalog-embed.md` |
| 3 | Client `Adapter`/`Target` + `For()` resolution + `Cost()` | `03-client-targets.md` |

Stage 1 is independent of the embed schema and can start immediately. Stages 2
and 3 depend on Stage 1 being merged.
