# Adapters

How Relay handles multiple inbound wire-shapes and translates between
them. Authoritative source for the codebase's adapter mechanics — when
this disagrees with the code, fix one or the other. The protocol-level
design (item taxonomy, streaming contract, codebase rules) lives in
[`canonical-protocol.md`](canonical-protocol.md).

## Concepts

- **Canonical** is `pkg/relay/v1/` — a narrowed-Responses-shape
  protocol that all vendor adapters target. Every cross-shape route
  goes through canonical; there are no pairwise translator packages.
- **Vendor adapter** is one folder under `pkg/adapters/<vendor>/`.
  Owns *every* wire shape that vendor serves (e.g. OpenAI owns CC,
  Responses, Embeddings as files within the same package).
- **Wire shape** is one HTTP API surface (e.g. `/v1/chat/completions`
  vs `/v1/messages`). Multiple wire shapes can share a vendor.
- **Spec** (`app/adapter/Spec`) bundles a wire shape's runtime
  metadata: name, inbound URL paths, upstream URL path, auth strategy,
  canonical `Translator`, token extractor, optional `IsNativePath`
  predicate, optional `BytePass` flag.

## File layout

```
pkg/relay/v1/                  CANONICAL protocol — zero imports of app/ or pkg/adapters/<vendor>/
  doc.go, types.go, request.go, response.go,
  items.go, parts.go, tools.go, events.go,
  parse.go, serialize.go, sse.go,
  translator.go, name.go, model_opts.go

pkg/adapters/openai/           OpenAI vendor — one folder, ALL OpenAI wire shapes
  adapter.go, types.go, parse.go, chat_request.go,
  context.go, tokens.go           ← Chat Completions wire
  responses_types.go, responses_request.go,
  responses_response.go, responses_items.go,
  responses_parts.go, responses_tools.go,
  responses_events.go, responses_parse.go,
  responses_serialize.go, responses_sse.go  ← Responses API wire
  translator_cc.go                ← v1.Translator for CC wire
  translator_responses.go         ← v1.Translator for Responses wire
                                     + NewComposedStream helper

pkg/adapters/anthropic/        Anthropic vendor
  adapter.go, types.go, parse.go,
  content.go, stream.go, tokens.go,
  transform.go, transform_response.go  ← Messages wire
  translator_canonical.go              ← v1.Translator

app/adapter/                   Generic framework (singular, NOT per-vendor)
  spec.go                      Spec, AuthStrategy, InboundPath, specAdapter
  registry.go                  Registry of Specs + accessors

app/adapters/                  Vocabulary only (collapse pending PR 5)
  name.go                      Name constants (catalog vocabulary)
  json.go                      shared marshal/unmarshal helpers
  translator.go                OLD adapters.Translator interface — UNUSED
                                 after Phase 2; PR 5 deletes the file

app/httpapi/inference/         Shape-agnostic
  dispatch.go                  Dispatch — no per-vendor branching
  mount_registry.go            generic MountRegistry(reg) RouteMounter
  mode.go, middleware.go, etc. shared concerns

cmd/relay/main.go              Composition root — the ONLY place
                                 vendor names appear in code form
```

## Routing

`/v1/chat/completions` and `/openai/v1/chat/completions` mount onto the
OpenAI CC spec. `/v1/responses` and `/openai/v1/responses` mount onto
the OpenAI Responses spec. `/v1/messages` and `/anthropic/v1/messages`
mount onto the Anthropic spec. `/v1/embeddings` and
`/openai/v1/embeddings` mount onto the Embeddings spec (BytePass).

Upstream shape is determined by `plan.HostBinding.Adapter`. Dispatch
decides the path:

1. Resolve inbound spec from `DispatchInput.Inbound`.
2. **If `Spec.BytePass`**: byte-pass to the inbound spec's own
   upstream URL. (Embeddings.)
3. **If `Spec.IsNativePath(plan)` is true**: byte-pass to the inbound
   spec's own upstream URL — used when the host natively speaks the
   inbound shape even though `HostBinding.Adapter` says otherwise.
   (Canonical case: OpenAI Responses inbound + `host="openai"`.)
4. **If `Inbound == plan.HostBinding.Adapter`**: same-shape byte-pass
   to the upstream spec's URL.
5. **Otherwise**: run the canonical chain via `dispatchCanonical`:
   ```
   inboundT.ParseRequest(body)  → canonical
   upstreamT.SerializeRequest(canonical) → wire body
   upstream.Call(...)
   upstreamT.ParseResponse(body) → canonical
   inboundT.SerializeResponse(canonical, originalRequest) → wire body
   ```
   Streams compose `NewToCanonicalStream()` (upstream) with
   `NewFromCanonicalStream()` (inbound).

## The Translator interface

```go
// pkg/relay/v1/translator.go
type Translator interface {
    ParseRequest(body []byte) (*Request, error)
    SerializeRequest(req *Request) ([]byte, error)
    ParseResponse(body []byte) (*Response, error)
    SerializeResponse(resp *Response, req *Request) ([]byte, error)
    NewToCanonicalStream() func(chunk []byte) ([]byte, error)
    NewFromCanonicalStream() func(chunk []byte) ([]byte, error)
}
```

Stateless across requests. Per-stream state lives in the closures
returned by the two stream factories. `req` argument on
`SerializeResponse` is passed so wire shapes that require
request-echo (OpenAI Responses) can render correctly without per-
request stateful translators.

## Known lossiness

Cross-vendor translation loses things. Tagged in code with
`// canonical:` near the drop site.

- **Vendor-specific opaque blobs** preserved via `provider_data
  json.RawMessage` on `Reasoning`, `FunctionCall`, `Message` items.
  Round-tripped within a vendor; dropped cross-vendor. (Anthropic
  thinking signatures, OpenAI `encrypted_content`.)
- **Cache hints** (Anthropic `cache_control`) dropped on the
  canonical round-trip. Future: live in `extensions` envelope. Doc
  open question.
- **Safety settings** (Gemini per-category thresholds): not modeled.
  Future: per-policy or in `extensions`.
- **Server tools** (web_search, code_execution, computer-use, etc.):
  wire-modeled in `Tool.kind: "server" | "mcp"` but runtime-rejected
  in v1. Doc roadmap section.
- **Anthropic `pause_turn` stop reason**: maps to
  `incomplete_details.reason: "pause_turn"`. v2 server-tool work will
  revisit.
- **OpenAI Responses stateful fields** (`previous_response_id`,
  `store`, `conversation`, `background`, etc.): rejected at parse —
  canonical is stateless.

## Adding a new shape

For a new vendor wire shape (e.g. Gemini Native, Bedrock Converse,
Cohere):

1. Add `pkg/adapters/<vendor>/` with:
   - The vendor wire-shape types (`types.go`, etc.).
   - `translator.go` (or `translator_<shape>.go`) implementing
     `v1.Translator`.
   - Token extraction in `tokens.go`.
2. Add the `Name` constant to `app/adapters/name.go`.
3. Register a `Spec` literal in `cmd/relay/main.go`:
   ```go
   (&adapter.Spec{
       Name:          adapters.Vendor,
       InboundPaths:  []adapter.InboundPath{...},
       UpstreamPath:  "/v1/...",
       Auth:          adapter.AuthStrategy{Header: "...", Scheme: "..."},
       Translator:    pkgvendor.Translator{},
       ExtractTokens: pkgvendor.ExtractTokens,
   }).Build(),
   ```
4. Add round-trip + composition unit tests in
   `pkg/adapters/<vendor>/translator_test.go`.

**Do not** create a vendor-specific package under `app/` or a pairwise
translator package. The canonical chain composes any A→B via inbound
+ upstream Translators automatically.

## Adding a new wire shape for an existing vendor

For e.g. Anthropic launching a Messages-v2 endpoint:

1. Add new files to the existing `pkg/adapters/<vendor>/` folder
   (e.g. `messages_v2_*.go`).
2. Add a new `translator_messages_v2.go` implementing `v1.Translator`.
3. Add a new `Spec` literal in `cmd/relay/main.go` with that
   translator + new inbound paths. The Spec is a separate registration
   but lives next to the existing vendor specs.
4. The catalog gains a new `adapter` value for HostBindings.

**Never** create a new folder per wire shape. Codebase rule 3: one
folder per vendor.
