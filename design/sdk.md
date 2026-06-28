# Relay Go SDK (`github.com/wyolet/relay/sdk`)

The `sdk/` module is the vendorable public client library. A consumer pulls it
alone — it never drags in the relay server, Postgres, Redis, or ClickHouse.

```
sdk/
  v1/                       canonical protocol: types, Translator, streams
  adapters/{openai,anthropic,gemini}/   pure vendor translators
  usage/                    Tokens + timing types
  catalog/                  embedded host/binding/pricing data + resolver + Cost
  client/                   the HTTP/WS client consumers call
  oauth/                    vendor-neutral OAuth: PKCE auth-code + device
                            (RFC 8628) + refresh, TokenSource (in-process
                            lifecycle + Persister), RFC 8414 discovery
```

## OAuth (`sdk/oauth`)

Generic OAuth machinery over `golang.org/x/oauth2` so consumers (apps and the
relay server) don't hand-roll PKCE/exchange/refresh and nobody is locked into
relay. Shipped in #349.

- `oauth.Flow` — `AuthorizeURL` (PKCE S256), `Exchange`, `DeviceAuth`/
  `DeviceToken` (RFC 8628), lazy `Refresh`. Machinery only: no persistence,
  no loop.
- `oauth.TokenSource` — standalone in-process token lifecycle: caches until
  expiry, refreshes on expiry, single-flights concurrent refreshes, persists
  the rotated token via a caller-supplied `Persister`. The relay-free path.
- `oauth.ProviderConfig` — serializable shape mapping to `*oauth2.Config`;
  vendor specifics are config, not baked in. `Discover` fills empty endpoints
  from RFC 8414 authorization-server metadata (explicit config wins).

Vendor specifics (client_id, endpoints, scopes, beta headers) are data, never
baked into the package — mirror the sanctioned Claude Agent-SDK auth in config.
Server-side use of these primitives lives in `pkg/secret/oauth` +
`app/hostkey` `ValueKindOAuth` (see `design/settings.md`).

## Module boundary

`sdk` depends on nothing of relay's server. The relay server module
(`github.com/wyolet/relay`) depends on `sdk` (via `replace ./sdk` + `go.work`
for in-repo dev). Direction is server → sdk, never reverse — the canonical
library is the foundation. Codebase rules 1/2/4/10 (canonical knows nothing;
vendors import canonical; no vendor names in `app/`; `pkg` purity) extend to the
module: nothing under `sdk/` imports `app/` or `internal/`.

## Releasing the modules

The repo is three modules — root `github.com/wyolet/relay` (the server, whose
`app/`/`pkg/` packages external repos like `relay-catalog` import), plus
`sdk` and `jobq` submodules. In-repo dev wires them via `go.work` + the
`replace github.com/wyolet/relay/sdk => ./sdk` / `./jobq` directives in the root
`go.mod`; those replaces are **dev-only** — Go ignores a dependency's `replace`
lines, so an external consumer resolves the root `go.mod`'s `require` versions
from real tags. The root `require`s therefore must point at **published**
versions, never `v0.0.0`.

`sdk` and `jobq` are versioned **independently** with their own tags
(`sdk/vX.Y.Z`, `jobq/vX.Y.Z`); the root releases on its own `vX.Y.Z` via
`make release`. This is manual — there is no automation tying them together,
so the discipline is:

1. When `sdk` or `jobq`'s API changes and you want consumers to get it, tag the
   submodule at the release commit: `git tag sdk/vX.Y.Z && git push origin sdk/vX.Y.Z`.
2. Bump the matching `require github.com/wyolet/relay/<mod> vX.Y.Z` in the root
   `go.mod` and commit. **Keep the `replace`** (it's what dev/`go.work` use).
   The root build stays green even before the tag exists because `go.work` +
   `replace` shadow the version — but the tag must exist before any external
   consumer (or a tagged root release) pulls it.
3. Release the root normally with `make release`.

Leaving a root `require` at `v0.0.0` (or a stale version) silently breaks
external consumers while in-repo builds keep working — the failure only shows up
downstream. Verify consumability after tagging by `go get`-ing the root module
from a scratch dir outside the repo.

## Calling relay (primary path)

```go
c := client.Relay(baseURL, relayKey)            // POST /v1/generate, key pooling, routing, limits
resp, err := c.Generate(ctx, req)               // req is a *v1.Request
```

Configurable via `WR_*` env (base URL, key, headers, timeout, usage echo) or
explicit `Option`s (`WithHTTPClient`, `WithHeader`, `WithAuth`, ...).

## Calling a catalogued upstream directly

`For(ref, apiKey)` resolves a model ref against the embedded catalog and wires
baseURL + adapter (translator/path/auth) + upstream wire name automatically.
Bypasses relay (no pooling/limits/observability) — local-dev / offline / SDK use.

```go
c, err := client.For("gpt-4o", openaiKey)              // bare ref
c, err := client.For("openai/gpt-4o", openaiKey)       // provider-qualified
c, err := client.For("gpt-4o@openai", openaiKey)       // host-pinned
```

Ref forms: `model`, `provider/model`, `model@host`, `provider/model@host`. An
ambiguous bare/qualified ref errors and lists the `@host` pins that disambiguate
(every host has one).

### Overrides

Each override swaps one whole, self-consistent piece — `path`/`auth`/translator
only ever move together as an `Adapter`, so you cannot produce a mismatched
wire call:

```go
client.For(ref, key,
  client.WithBaseURL("https://proxy.internal"),  // endpoint only
  client.WithAdapterName("anthropic"),           // whole wire bundle
  client.WithUpstreamModel("gpt-4o-2024-08-06"), // wire model name
  client.WithClient(client.WithHeader("X-Org", "acme")), // client Options
)
```

`OpenAI()/Anthropic()/Gemini()/New()` remain for explicit off-catalog targets.

## Cost

For catalog-resolved clients hitting a host that carries pricing, cost is
computed from the response's token usage:

```go
cost, ok := resp.Cost()    // ok=false for relay targets, unpriced hosts
cost, ok := stream.Cost()  // valid after the stream's terminal usage event
```

Pricing reflects the resolved catalog binding; it does not follow a
`WithBaseURL` override to a genuinely different upstream.

## Regenerating the embedded catalog

`sdk/catalog/catalog.json` is generated, not hand-edited:

```
make catalog-embed        # reads $RELAY_CATALOG_DIR (or ../relay-catalog/data)
```

`cmd/catalog-embed` composes the public catalog (drafts excluded via
`manifest.LoadDir`), flattens it to the embed schema, and validates that every
adapter name maps to a registered SDK adapter — an unknown adapter fails the
build rather than shipping a broken client.
