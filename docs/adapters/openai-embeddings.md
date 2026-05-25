# OpenAI Embeddings — Canonical Fidelity Audit

> Audited 2026-05-25 against the `openai_embeddings` Spec in `cmd/relay/main.go` and `app/httpapi/inference/dispatch.go`. There is no embeddings translator.

## Verdict

**Not applicable — pure byte-pass, no canonical mapping exists.** The
embeddings shape is registered with `BytePass: true` and **no
`Translator`**. There is no canonical embeddings type in `pkg/relay/v1`
(canonical models text generation only). Requests and responses are
forwarded verbatim to the upstream `/v1/embeddings` with the model field
rewritten; nothing is parsed into or out of canonical, so there is no
fidelity loss *and* no cross-shape capability.

## What actually happens

- `dispatch.go` short-circuits on `inboundSpec.BytePass`: it calls
  `runBytePass` with the inbound spec's own adapter. The body is passed
  through after `rewriteModelField(body, snapshot.Upstream())` — the only
  transformation.
- **Hard constraint** (`dispatch.go:101-106`): the resolved host's
  `HostBinding.Adapter` MUST be `openai`. Any other adapter →
  `400 embeddings_unsupported_host`. This is permanent: Anthropic/Gemini
  expose no OpenAI-compatible `/v1/embeddings`, and there is no canonical
  embeddings shape to translate through.
- Streaming is force-disabled (`handleShape`: `if spec.BytePass { stream = false }`).
- `ExtractTokens` runs (`pkgopenai.ExtractTokens`) so usage is still
  observed for billing.

## ⚠️ Silently dropped

Nothing relay-side — the body is opaque. Whatever the upstream supports
(dimensions, encoding_format, user, etc.) passes through untouched
because relay never inspects it. This is the one shape where "we don't
model it" is the *correct* and complete behavior.

## Recommendations

- **None for fidelity.** The byte-pass is honest.
- If a non-OpenAI embeddings provider (Voyage native, Cohere native
  embed) is ever onboarded with a *different* wire shape, it needs its
  own Spec, not this one. A canonical embeddings type would only be worth
  building if cross-provider embeddings translation becomes a real
  requirement — currently out of scope (`canonical-protocol.md`).
