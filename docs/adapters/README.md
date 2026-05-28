# Adapter Fidelity Audits

Per-adapter, honest maps of what survives translation to/from the
canonical protocol (`sdk/v1`) and what is **silently dropped**,
**hardcoded**, or **unsupported**. Mechanics live in
[`../adapters.md`](../adapters.md); protocol design in
[`../canonical-protocol.md`](../canonical-protocol.md). These docs are
the fidelity contract — when one disagrees with the code, fix one or the
other.

Audited 2026-05-25 (post PR #200). Each finding cites `file:line`.

| Adapter | Doc | Direction support | Overall |
|---|---|---|---|
| OpenAI Chat Completions | [openai-chat-completions.md](openai-chat-completions.md) | inbound + upstream | good — audit gaps fixed (#202) |
| OpenAI Responses | [openai-responses.md](openai-responses.md) | inbound + upstream (byte-pass when host is openai-proper) | good — audit gaps fixed (#202) |
| Anthropic Messages | [anthropic.md](anthropic.md) | inbound + upstream | good — audit gaps fixed (#202 + max_tokens default); Output.Format still open |
| Google Gemini | [gemini.md](gemini.md) | **upstream only** (undogfooded — no model access) | fair — audit gaps fixed (#203); provider_data/native-inbound open |
| OpenAI Embeddings | [openai-embeddings.md](openai-embeddings.md) | byte-pass only | n/a — no translation, honest passthrough |

## Cross-cutting findings (the patterns that repeat)

These are gaps that appear in **multiple** adapters — i.e. canonical
surface that no/few adapters honor. Fix these at the canonical/contract
level, not one adapter at a time.

### Structured output (`ModelConfig.Output.Format`) is dropped everywhere
- **Anthropic**: never read (`translator_canonical.go:287-387`).
- **Gemini**: never read; `responseSchema`/`responseMimeType` never emitted (`translator_canonical.go:169-214`).
- A caller asking for `json_schema` output gets unformatted text, silently, on both. Canonical models the field but ~no adapter implements it.

### `provider_data` round-trip is incomplete (breaks multi-turn reasoning)
- **OpenAI Responses**: `encrypted_content` dropped despite a comment claiming it's stored (`translator_responses.go:447-449`).
- **Anthropic (streaming)**: thinking `signature_delta` has no handler; streamed `Reasoning.ProviderData` is always nil (`translator_canonical.go:809-825`).
- **Gemini**: `thoughtSignature` never populated or read.
- Net: same-vendor multi-turn extended-thinking continuations silently break whenever the prior turn arrived via streaming or cross-shape.

### Streaming usage is unreliable
- **OpenAI CC**: never sets `stream_options.include_usage`, so `generation.completed` usage is empty on virtually every stream (`translator_cc.go:289-292`).
- Pricing/observability get zero tokens for streaming requests on CC.

### Sampling parameters dropped without signal
- **OpenAI Responses**: `Seed`, `FrequencyPenalty`, `PresencePenalty` have no wire field — dropped silently.
- **Gemini**: emits `frequencyPenalty`/`presencePenalty`/`seed` even where the model may reject them (verify per-model).
- No adapter logs when it drops a sampling param it can't express.

### `max_tokens` defaulting — FIXED
- ~~**Anthropic** hardcodes `4096` when canonical `MaxTokens` is nil~~ → dispatch now seeds the default from the catalog model's `MaxOutputTokens` (`app/httpapi/inference` `applyOutputDefaults`); the 4096 constant is only a last-resort fallback. Vendor-neutral.

## Severity-ranked action list

> **Status (2026-05-25):** items 1–15 below were FIXED in PRs #202 / #203
> (and the `max_tokens` default), each with regression tests. The P2
> structural items are partially addressed; remaining open gaps are
> Anthropic `Output.Format` (#16) and the no-silent-drops contract (see
> bottom). Kept here as the historical findings record — verify against
> git log before treating any as still-open.

**P0 — correctness bugs (wrong/corrupt output or panic) — all FIXED:**
1. **OpenAI CC** `NewFromCanonicalStream()` returns `nil` (`translator_cc.go:436-438`). Comment claims "not a production path" but inbound CC streaming against a non-OpenAI upstream IS that path → nil-deref risk. **Verify and fix or guard.**
2. **Gemini** streaming tool-call args wrapped with `fmt.Sprintf("%q", …)` → emits a JSON *string* where Gemini expects an *object*; `name` also empty (`translator_canonical.go:~688`). Every canonical→Gemini streamed function call is malformed.
3. **Anthropic** streaming thinking `signature_delta` never accumulated → multi-turn extended thinking breaks (`translator_canonical.go:809-865`).
4. **OpenAI Responses** streaming refusal (`response.refusal.delta/done`) and `response.failed` silently dropped → empty stream / hung consumer (`translator_responses.go:889`).
5. **OpenAI Responses** from-canonical function-call streaming emits empty `call_id`/`name` (`translator_responses.go:976-979,1133-1145`).
6. **Gemini** safety `finishReason`s (BLOCKLIST, PROHIBITED_CONTENT, SPII, MALFORMED_FUNCTION_CALL, LANGUAGE) fall through to `stop` → blocked responses look like normal completions (`translator_canonical.go:~1103`).

**P1 — silent data loss / observability gaps — all FIXED:**
7. **OpenAI CC** URL-citation annotations dropped (`translator_cc.go:742-784`).
8. **OpenAI CC** audio/prediction usage keys dropped in per-request path (divergent from `ExtractTokens`).
9. **OpenAI CC** streaming usage always empty (`stream_options` not set).
10. **Anthropic** `ToolsConfig.Parallel` is dead code — parallel tool use can't be disabled (`translator_canonical.go:350`).
11. **Anthropic** `stop_sequence` matched string dropped (sync + stream).
12. **Anthropic** `max_tokens` 4096 silent cap → now defaulted from catalog model max.
13. **Gemini** parallel tool-call `CallID` collision (CallID = function name) breaks call→result pairing.
14. **OpenAI Responses** `file_citation` annotations dropped (`translator_responses.go:490-502`).
15. **OpenAI CC** canonical `Response.Error` not serialized → looks like empty success (`translator_cc.go:341-423`).

**P2 — structured-output + cache + cross-cutting (see above):**
16. `Output.Format` dropped by Anthropic + Gemini.
17. `provider_data` round-trip incomplete (Responses/Anthropic-stream/Gemini).
18. Gemini `CacheConfig`/`ItemCacheConfig.Anchor` ignored (correct — no inline breakpoint — but no caller signal).
19. Responses sampling params (`Seed`/penalties) + reasoning (`Summary`/`BudgetTokens`) dropped.

## Recommended policy change

The recurring theme is **silent** drops. Adopt a contract rule: an
adapter that receives canonical input it cannot express MUST either (a)
emit it on the wire, (b) log a structured warning (`adapter_drop` with
field name), or (c) return an explicit error for safety-relevant fields
(e.g. unmapped safety/finish reasons). "Accept and discard with no
signal" should be banned. Tracking: add to `roadmap.md` and open issues
per P0.
