# Stage 3 — client Adapter / Target / For() / Cost() (EPHEMERAL)

**PR:** `feat/sdk-client-catalog` · depends on Stages 1 & 2 merged.

## Adapter registry (refactor existing constructors)

`sdk/client` already hardcodes the three wire bundles inside `OpenAI()`,
`Anthropic()`, `Gemini()`. Extract them into named `Adapter` values keyed by
adapter name — the single source of truth for *how* to call each shape.

```go
// Adapter is the atomic wire bundle. translator+path+auth always travel
// together; selecting an adapter swaps all three at once.
type Adapter struct {
    translator v1.Translator
    pathFn     func(model string, stream bool) string
    auth       Auth        // {Header, Scheme}
}

var adapters = map[string]Adapter{
    "openai":    {openai.Translator{}, staticPath("/v1/chat/completions"), Auth{"Authorization","Bearer"}},
    "anthropic": {anthropic.Translator{}, staticPath("/v1/messages"),        Auth{"x-api-key",""}},
    "gemini":    {gemini.Translator{},    geminiPath,                         Auth{...}},
}
```

Keep `OpenAI()/Anthropic()/Gemini()/New()` as-is for source compatibility; they
become thin wrappers that pick the named adapter. **Name is `Adapter`, not
`Shape`** (matches catalog field + relay's `adapter.Spec`).

## Target

```go
// Target is a fully-resolved, self-consistent upstream call config. Immutable.
type Target struct {
    baseURL  string
    adapter  Adapter
    upstream string        // wire model name to send
    pricing  []catalog.Rate
}
```

Produced by catalog resolution or built manually. Overrides return a *new*
Target with one whole piece swapped — never a half-mutated config.

## Constructors

```go
// For resolves a model ref against the embedded catalog and returns a wired
// client. ref: "gpt-4o" | "openai/gpt-4o" | "gpt-4o@openai-direct".
func For(ref, apiKey string, opts ...Option) (*Client, error)

// Overrides (each yields a consistent Target):
WithBaseURL(url string)      // proxy/gateway; adapter+auth unchanged
WithAdapterName(name string) // off-catalog: atomic bundle swap
WithUpstreamModel(name string)
```

`For` calls `catalog.Load().Resolve(ref)`, builds the `Target`, applies opts,
constructs the `Client`. Ambiguous ref ⇒ error (from resolver) surfaced via the
existing deferred-config-error path (`configErr`).

`Relay(...)` (the existing default-target constructor) is unchanged — relay is
still the primary target and does not go through catalog resolution.

## Cost surfacing

The response/stream already carry `usage.Tokens`. Add cost accessors that defer
to the Target's pricing:

```go
func (r *Response) Cost() (float64, bool)   // ok=false when Target unpriced
func (s *Stream)   Cost() (float64, bool)   // valid after stream drains usage
```

Implementation calls `catalog.Cost(target.pricing, tokens)` (the pure tier loop
from Stage 2). For `Relay()` targets (no catalog pricing attached) ⇒ `ok=false`
— relay-side cost echo (`X-WR-Usage`, see echo-usage memory) remains the path
for relay calls; direct-to-vendor is where SDK-side Cost() earns its keep.

## Override-safety invariant (the footgun killer)

There is **no API that sets a path or translator independently of an adapter.**
`path`/`auth`/`translator` are only ever set as a unit via an `Adapter`. So a
consumer cannot point the anthropic translator at an OpenAI URL: changing the
wire shape means `WithAdapterName`, which swaps the whole bundle atomically.
`WithBaseURL` only moves the endpoint. This is the structural guarantee that
"catalog and code match exactly" — code owns the bundle, catalog owns selection.

## Acceptance gates

- `client.For("gpt-4o@<host>", key)` makes a correct direct call (smoke against
  `make smoke-mock` recorded fixtures where possible).
- `resp.Cost()` matches `app/pricing.Cost` for a priced host; `ok=false` for an
  unpriced host and for `Relay()` targets.
- No exported API sets path/translator without an adapter (review gate).
- `sdk` module still pulls zero `app/` / heavy deps.
- Public API documented in the soon-to-be-permanent `docs/sdk.md`.

## Distillation (end of initiative)

After Stage 3 merges:
- Fold the module-boundary rules into `docs/canonical-protocol.md`
  ("vendorable library is now the `sdk` module; server depends on it").
- Write `docs/sdk.md` (public consumer guide: install, `For()`, overrides, Cost).
- Delete `docs/sdk-spec/`.
- Update memory: new `sdk` module topology, embed pipeline, `make catalog-embed`.
