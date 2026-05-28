# Stage 2 — catalog embed pipeline (EPHEMERAL)

**PR:** `feat/sdk-catalog-embed` · depends on Stage 1 merged.

## Two pieces

1. `sdk/catalog/` — pure pkg: mirror structs + `//go:embed catalog.json` +
   resolver + `Cost()`. Zero `app/` imports.
2. `cmd/catalog-embed/` — server-module tool: reads catalog via `app/seed` +
   `app/manifest`, flattens, validates against the SDK adapter registry, writes
   `sdk/catalog/catalog.json`.

## Embed JSON schema (hosts + bindings + pricing)

Per the per-binding decision. The embed is a list of hosts; each host carries
its model bindings and (optional) pricing. Mirror structs in `sdk/catalog` are
**plain** — no `app/meta`, no validators, no UUIDs the consumer doesn't need.

```jsonc
{
  "version": "relay-catalog@v1alpha2",        // RELAY_CATALOG_VERSION at gen time
  "generatedAt": "2026-05-28T...Z",
  "hosts": [
    {
      "name": "openai-direct",                 // selection key for @host
      "baseURL": "https://api.openai.com",
      "models": [
        {
          "model": "gpt-4o",                   // catalog slug (the bare ref)
          "providers": ["openai"],             // for provider/model disambig only
          "adapter": "openai",                 // join key → SDK Adapter registry
          "upstream": "gpt-4o-2024-08-06",     // wire name to send (OriginalName)
          "pricing": [                          // omitted/empty ⇒ no Cost()
            {"meter":"tokens.input","unit":"per_million","amount":2.5,"aboveTokens":0},
            {"meter":"tokens.output","unit":"per_million","amount":10,"aboveTokens":0}
          ]
        }
      ]
    }
  ]
}
```

Go mirror types (pure):

```go
package catalog

type Catalog struct { Version string; Hosts []Host }
type Host    struct { Name, BaseURL string; Models []Binding }
type Binding struct {
    Model, Adapter, Upstream string
    Providers []string
    Pricing   []Rate          // nil/empty ⇒ unpriced
}
type Rate struct { Meter, Unit string; Amount float64; AboveTokens int }
```

Pricing is resolved per (model, host): for each binding, find the host's enabled
`Pricing` whose `TargetModelIDs` include this model, inline its `Rates`. If none,
`Pricing` stays nil.

## `Cost()` — pure copy of app/pricing.Cost

`sdk/catalog` reimplements the ~25-line tier loop (cannot import `app/pricing`):

```go
// Cost returns total cost in the rate sheet's currency and ok=false when the
// binding carries no pricing. Tier axis = input tokens (matches app/pricing).
func (b Binding) Cost(t usage.Tokens) (float64, bool)
```

Reuse `app/pricing`'s `MeterForUsageKey` mapping verbatim (copy the switch).
Keep a test that pins parity with `app/pricing.Cost` on a shared fixture so the
two never silently diverge (parity test lives in the server module where both
are importable).

## Resolver (in `sdk/catalog`)

Mirror relay's alias index (see `app/routing` + model-resolution memory). Build
once at init from the embedded data:

```go
func Load() (*Catalog, error)              // unmarshal embedded bytes
func (c *Catalog) Resolve(ref string) (Binding, Host, error)
//   ref forms: "gpt-4o" | "openai/gpt-4o" | "gpt-4o@openai-direct"
//   ambiguous bare/provider ref across multiple hosts ⇒ error listing candidates
```

O(1) maps: `bare→[]bindingRef`, `provider/model→[]bindingRef`,
`model@host→bindingRef`. Ambiguity is an explicit error, not a silent pick
(the consumer then disambiguates with `@host`).

## `cmd/catalog-embed` (server module)

1. `seed`-style load of the catalog dir (`RELAY_CATALOG_DIR`), reusing
   `manifest.LoadDir` (already skips `drafts/` + `*.draft.yaml`) and the seed
   compose/resolver so cross-refs (model ids, pricing target ids) resolve.
2. Flatten composed domain → embed schema above.
3. **Adapter drift gate:** for every binding's adapter name, assert it exists in
   the SDK adapter registry (Stage 3 defines the registry; until then, assert
   against the literal set `{openai,anthropic,gemini}`). Unknown ⇒ non-zero exit.
4. Marshal deterministically (sorted keys/slices) → write
   `sdk/catalog/catalog.json`.

`make catalog-embed` target wraps it. Commit the generated JSON. Document that
it's generated (header comment / `//go:generate` note) and regenerated on
catalog bumps.

## Acceptance gates

- `sdk/catalog` has zero `app/`/`internal/` imports; `go list -deps` clean.
- `make catalog-embed` produces a stable (idempotent) `catalog.json`.
- Parity test: `catalog.Binding.Cost == app/pricing.Pricing.Cost` on fixtures.
- Drafted catalog items never appear in the embed (test with a `*.draft.yaml`).
- Unknown adapter in catalog ⇒ `cmd/catalog-embed` exits non-zero.
