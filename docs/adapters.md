# Adapters

How Relay handles multiple inbound wire-shapes (OpenAI, Anthropic, future
Gemini, etc.) and translates between them. Source of truth — when this
disagrees with code, fix one or the other.

## Concepts

- **Adapter** — one wire protocol. Identified by `adapters.Name`
  (`"openai"`, `"anthropic"`, …). Currently both an *inbound* shape (a
  customer-facing HTTP surface) and an *upstream* shape (how Relay talks
  to a provider). The two roles share the same Name when symmetric, which
  they are today.
- **OpenAI is the canonical hub** until we build a richer internal shape.
  Every non-OpenAI adapter implements four functions against OpenAI's
  types; OpenAI itself is identity at the hub.
- **Bare `/v1/...` paths are reserved** for the future canonical shape.
  During the transition (Path A), bare paths *also* serve OpenAI shape
  for backwards-compat — they migrate to canonical when canonical lands.

## File layout

```
pkg/adapters/                  pure shape, no Relay imports, no chi
  openai/                      types, parse, tokens
  anthropic/                   types, parse, transform, stream, tokens
  <vendor>/                    (future)

app/adapters/                  Relay glue; may import chi/huma
  name.go                      Name enum + Valid() + All()
  registry.go                  Adapter interface, Registry, Translator interface
  openai/
    adapter.go                 pipeline.Adapter impl (Call, Retryable, ExtractTokens)
    routes.go                  MountRoutes — owns `/openai/v1/...` AND bare `/v1/...`
    translator.go              identity Translator
  anthropic/
    adapter.go
    routes.go                  MountRoutes — owns `/anthropic/v1/messages`
    translator.go              wraps pkg/adapters/anthropic transforms

app/httpapi/inference/         shape-agnostic
  inference.go                 Mount(api, deps): loops adapter registry, calls MountRoutes
  proxy.go                     shared proxy handler used by every shape's routes
  models.go                    `/v1/models` and `/<shape>/v1/models` listing (catalog-driven)
  errors.go, middleware.go     shared helpers
```

No file under `app/httpapi/inference/` is named after a shape. All
shape-named files live under `app/adapters/<name>/`.

## Routing rules

- `/openai/v1/chat/completions` — inbound shape locked to OpenAI.
- `/anthropic/v1/messages` — inbound shape locked to Anthropic.
- `/v1/chat/completions` — bare; **today** serves OpenAI shape (Path A
  transitional). Will migrate to canonical shape when canonical lands.
- `/v1/messages` — bare; will be Anthropic-on-canonical in the future;
  not mounted today.
- Catalog endpoints `/v1/models`, `/<shape>/v1/models` — shape-scoped
  listings, snapshot-driven.

Upstream shape is determined by `plan.HostBinding.Adapter`. Translation
matrix:

| inbound (route) | upstream (binding) | path |
|---|---|---|
| openai | openai | passthrough; identity translator |
| anthropic | anthropic | passthrough; identity translator |
| openai | anthropic | inbound.ToOpenAI = identity, anthropic.FromOpenAI on body, anthropic.ToOpenAIResponse on response |
| anthropic | openai | anthropic.ToOpenAI on body, upstream.FromOpenAI = identity, anthropic.FromOpenAIResponse on response |
| openai | gemini | inbound identity, gemini.FromOpenAI, gemini.ToOpenAIResponse, inbound identity |
| anthropic | gemini | anthropic.ToOpenAI, gemini.FromOpenAI, gemini.ToOpenAIResponse, anthropic.FromOpenAIResponse |

Non-OpenAI ↔ non-OpenAI pays two translations. That's the cost of
pairwise-with-hub; canonical resolves it eventually.

## The Adapter interface

```go
// app/adapters/registry.go

type Adapter interface {
    Name() Name
    MountRoutes(api huma.API, deps Deps)
    Translator() Translator
    Upstream() pipeline.Adapter   // Call, Retryable, ExtractTokens
}

type Translator interface {
    ToOpenAIRequest(body []byte) (*openai.FullChatRequest, error)
    FromOpenAIRequest(*openai.FullChatRequest) ([]byte, error)
    ToOpenAIResponse(body []byte) (*openai.ChatResponse, error)
    FromOpenAIResponse(*openai.ChatResponse) ([]byte, error)
    NewToOpenAIStream() func(chunk []byte) ([]byte, error)
    NewFromOpenAIStream() func(chunk []byte) ([]byte, error)
}
```

The OpenAI translator is the identity for the four request/response
functions and returns nil chunk-transform funcs (caller skips the
transform when nil).

## Pipeline flow

The pipeline doesn't know which shape was inbound. Each route hands it
both adapters (inbound translator + upstream Adapter):

```
1. Route parses inbound body, resolves model, builds Plan.
2. Route constructs pipeline.Request with:
     Inbound:  registry[inboundName].Translator()
     Upstream: registry[plan.HostBinding.Adapter].Upstream()
     UpstreamTranslator: registry[plan.HostBinding.Adapter].Translator()
3. Pipeline.Run:
   a. canonicalReq, _ = Inbound.ToOpenAIRequest(body)        // identity if inbound=openai
   b. wireBody, _    = UpstreamTranslator.FromOpenAIRequest(canonicalReq)  // identity if upstream=openai
   c. resp           = Upstream.Call(ctx, baseURL, key, wireBody, hdr)
   d. if stream:
        chain stream transformers: UpstreamTranslator.NewToOpenAIStream → Inbound.NewFromOpenAIStream
      else:
        canonicalResp, _ = UpstreamTranslator.ToOpenAIResponse(respBody)
        outBody, _       = Inbound.FromOpenAIResponse(canonicalResp)
   e. write outBody / piped stream to client
   f. post-flight (detached goroutine): ExtractTokens from raw response
```

When inbound = upstream, both translators are identity → byte-equivalent
passthrough.

## Known lossiness (pairwise-with-hub)

Tracked here so canonical knows what to model when it arrives. Each item
is tagged in code with `// canonical:` near the drop site.

- `openai → anthropic`: drops `frequency_penalty`, `presence_penalty`,
  `n`, `seed`, `logit_bias`, `logprobs`, `top_logprobs`, `service_tier`,
  `reasoning_effort`, `store`, `response_format`, `parallel_tool_calls`.
- `openai → anthropic`: multimodal `image_url` parts pass through raw —
  Anthropic's image block shape is different. **Fixing this is part of
  the cross-shape PR**, not deferred.
- `anthropic → openai`: drops `cache_control` (prompt caching), Anthropic
  image source shape vs OpenAI image_url.
- `tool_choice: "none"` (OpenAI) → no Anthropic equivalent, dropped.
- All shape-specific provider extensions (anything outside the canonical
  spec) drop at the hub.

## Adding a new shape

```
pkg/adapters/<vendor>/         types.go, parse.go, transform.go,
                                stream.go, tokens.go
app/adapters/<vendor>/         adapter.go, routes.go, translator.go
```

Register in `cmd/relay/main.go`:

```go
registry := adapters.NewRegistry(
    openai.New(),
    anthropic.New(),
    vendor.New(),
)
```

Add `Name<Vendor>` constant in `app/adapters/name.go` and append to
`All()`.
