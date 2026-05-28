# OpenAI Responses API — Canonical Fidelity Audit

> Audited 2026-05-25 against `sdk/adapters/openai/translator_responses.go` (1301 lines)
> plus `responses_{types,request,response,items,parts,tools,events,parse,serialize,sse}.go`.
> Critical, honest map. **NOTE: when the inbound host is `openai` and the adapter name is
> `openai` (i.e., `plan.Host.Meta.Name == "openai"` and `plan.HostBinding.Adapter == "openai"`),
> `IsNativePath` returns true and dispatch byte-passes — `cmd/relay/main.go:192-195`. These
> findings apply to CROSS-SHAPE routing only (canonical → upstream, or non-OpenAI upstream →
> Responses inbound caller).**

---

## Verdict

The adapter is the tightest mapping in the codebase because canonical is a narrowed Responses
shape. The core happy path (text generation, function calls, reasoning, usage) is high-fidelity.
There are, however, **five material silent-drop classes** and **two structural bugs** in the
from-canonical stream that will surface data loss in production cross-shape routes.

---

## Request: canonical → Responses (`SerializeRequest`)

| Canonical element | Status | Notes + file:line |
|---|---|---|
| `Model` (first element) | ✅ | `translator_responses.go:87` — only `req.Model[0]` emitted; multiplexing silently sends only first model |
| `Instructions` | ✅ | `translator_responses.go:89` |
| `Input[]` items | ✅ | Loop via `responsesItemFromCanonical`, `translator_responses.go:51-61` |
| `ModelConfig[model].Sampling.Temperature` | ✅ | `translator_responses.go:348` |
| `ModelConfig[model].Sampling.TopP` | ✅ | `translator_responses.go:349` |
| `ModelConfig[model].Sampling.MaxTokens` | ✅ | `translator_responses.go:350` → `max_output_tokens` |
| `ModelConfig[model].Sampling.Stop` | ✅ | `translator_responses.go:351` → `stop_sequences` |
| `ModelConfig[model].Sampling.TopK` | ⚠️ | Parsed from Responses into canonical (`translator_responses.go:253`) but **dropped on round-trip back**: the `canonicalToResponsesRequest` note at line 352 says "TopK not in v1 canonical sampling params — omit". `SamplingParams` does have `TopK *int` (`model_opts.go:28`); the drop is in the outbound path only. |
| `ModelConfig[model].Sampling.Seed` | ⛔ | **Not in `ResponsesRequest` struct** (`responses_request.go:8-43`); not emitted. No error. |
| `ModelConfig[model].Sampling.FrequencyPenalty` | ⛔ | Same — not a Responses API field; not emitted, no error. |
| `ModelConfig[model].Sampling.PresencePenalty` | ⛔ | Same — not a Responses API field; not emitted, no error. |
| `ModelConfig[model].Reasoning.Effort` | ✅ | `translator_responses.go:355` |
| `ModelConfig[model].Reasoning.Summary` | ⛔ | **Silently dropped.** `ResponsesReasoningConfig` has only `Effort` (`responses_request.go:60-63`); `v1.ReasoningConfig.Summary` and `.BudgetTokens` are never written. |
| `ModelConfig[model].Reasoning.BudgetTokens` | ⛔ | Same as above — no field in wire struct, no error, no log. |
| `ModelConfig[model].Output.Format` | ✅ | `translator_responses.go:357-364` → `text.format` |
| `ModelConfig[model].Output.Verbosity` | ⛔ | **Silently dropped.** `v1.OutputConfig.Verbosity` has no Responses wire equivalent; never read in `canonicalToResponsesRequest`. |
| `ModelConfig[model].Tools.Definitions` (FunctionTool) | ✅ | `translator_responses.go:366-383` |
| `ModelConfig[model].Tools.Definitions` (ServerTool) | ⚠️ | `translator_responses.go:368-371` — non-`*v1.FunctionTool` entries are silently **skipped** (`continue`) with no error and no log. |
| `ModelConfig[model].Tools.Definitions` (MCPTool) | ⚠️ | Same silent skip — `translator_responses.go:368-371`. |
| `ModelConfig[model].Tools.Parallel` | ✅ | `translator_responses.go:383` |
| `ModelConfig[model].Tools.Choice` | ✅ | `translator_responses.go:384-389` |
| `CacheConfig` (Instructions / Tools) | ⛔ | **Silently dropped.** Responses API ignores explicit cache breakpoints (OpenAI caches automatically). Per `cache.go` adapter contract, this is intentional — but the drop is invisible (no log). |
| `ItemCacheConfig.Anchor` (per-item) | ⛔ | **Silently dropped.** `responsesItemFromCanonical` builds `ResponsesMessage` without consulting `Message.CacheConfig` (`translator_responses.go:515-519`). |
| `Message.ProviderData` (input items) | ⛔ | **Silently dropped.** `responsesItemFromCanonical` for `*v1.Message` does not copy `ProviderData` (`translator_responses.go:506-520`). |
| `FunctionCall.ProviderData` | ⛔ | Same — `translator_responses.go:522-529` constructs `ResponsesFunctionCall` without `ProviderData`. |
| `Reasoning.ProviderData` | ⛔ | Same — `translator_responses.go:544-556`. `encrypted_content` is not populated from `ProviderData`. |
| `User` | ✅ | `translator_responses.go:100` |
| `Metadata` | ✅ | `translator_responses.go:99` |
| `Extensions` | ⛔ | **Silently dropped.** `wireReq` struct in `SerializeRequest` has no extensions field; `req.Extensions` is never read (`translator_responses.go:64-101`). |
| `OutputMode` (stream) | ✅ | `translator_responses.go:339-343` → `stream` bool |
| Multi-model (`len(Model) > 1`) | ⚠️ | `req.Model[0]` is used silently; no error is returned if multiplexed. `ErrMultiplexNotImplemented` is not checked here. |

---

## Request: Responses → canonical (`ParseRequest`)

| Responses field | Status | Notes + file:line |
|---|---|---|
| `model` | ✅ | `translator_responses.go:222` |
| `input` (string norm) | ✅ | `responses_parse.go:131-146` |
| `input` (item array) | ✅ | `responsesUnmarshalItems` → `responsesItemToCanonical` |
| `instructions` | ✅ | `translator_responses.go:223` |
| `temperature`, `top_p` | ✅ | `translator_responses.go:256-257` |
| `top_k` | ⚠️ | Parsed into `SamplingParams.TopK` (`translator_responses.go:253`) but `v1.SamplingParams` has `TopK *int` (`model_opts.go:28`) so it IS representable canonically — however `canonical → Responses` drops it on the return path (see above). |
| `max_output_tokens` | ✅ | `translator_responses.go:258-260` |
| `stop_sequences` | ✅ | `translator_responses.go:261` |
| `tools` (function only) | ✅ | `translator_responses.go:267-294` |
| `tools` (built-in: web_search_preview, file_search, computer_use_preview, etc.) | ⛔ | `responsesUnmarshalTool` returns hard error for any non-`function` type (`responses_tools.go:86`). Caller maps to 400. **Explicit rejection, not silent drop.** |
| `tool_choice` | ✅ | `translator_responses.go:286-291` |
| `parallel_tool_calls` | ✅ | `translator_responses.go:285` |
| `reasoning.effort` | ✅ | `translator_responses.go:298-300` |
| `reasoning.effort` → `ReasoningConfig.Summary` | ⛔ | `ResponsesReasoningConfig` has no `summary` field (`responses_request.go:60-63`); Responses API does have a `summary` field (`"auto"/"concise"/"detailed"`) — it's simply not modeled. |
| `text.format` | ✅ | `translator_responses.go:304-315` |
| `stream` | ✅ | `translator_responses.go:229-233` |
| `user` | ✅ | `translator_responses.go:224` |
| `metadata` | ✅ | `translator_responses.go:225` |
| `logprobs` / `top_logprobs` | ⛔ | Parsed into `ResponsesRequest` fields (`responses_parse.go:102-104`) but **never mapped** into canonical. No canonical equivalent; silently discarded after parse — no error. |
| `previous_response_id` | ⛔ | **Explicit error** (`translator_responses.go:187-189`). |
| `store` (true) | ⛔ | **Explicit error** (`translator_responses.go:190-192`). `store: false` passes through silently (not rejected). |
| `conversation` | ⛔ | **Explicit error** (`translator_responses.go:193-195`). |
| `background` (true) | ⛔ | **Explicit error** (`translator_responses.go:196-198`). |
| `truncation` | ⛔ | **Explicit error** (`translator_responses.go:199-201`). |
| `service_tier` | ⛔ | **Explicit error** (`translator_responses.go:202-204`). |
| `safety_identifier` | ⛔ | **Explicit error** (`translator_responses.go:205-207`). |
| `prompt_cache_key` | ⛔ | **Explicit error** (`translator_responses.go:208-210`). |
| `context_management` | ⛔ | **Explicit error** (`translator_responses.go:211-214`). |
| `include` | ⛔ | **Explicit error** (`translator_responses.go:215-217`). |
| Message `cache_config` (per-item) | ⛔ | **Silently dropped.** `responsesItemToCanonical` for `*ResponsesMessage` does not copy any per-item cache config (`translator_responses.go:399-415`). (The Responses API does not have a per-item `cache_config` field today, but per canonical design this would arrive via `ItemCacheConfig.Anchor`.) |
| `ResponsesReasoning.EncryptedContent` | ⛔ | **Silently dropped.** Comment at `translator_responses.go:447-449` says "stored in ProviderData for same-vendor round-trip if needed (not modeled here in v1)" — but no actual storage into `ProviderData` is done. The field is parsed, then discarded. |
| `ResponsesFilePart.MediaType` | ⛔ | **Silently dropped.** `responsesPartToCanonical` for `*ResponsesFilePart` does not copy `MediaType` to `v1.FilePart.MediaType` (`translator_responses.go:472-479`). |
| `ResponsesFileCitationAnnotation` | ⛔ | **Silently dropped.** `responsesAnnotationToCanonical` handles only `*ResponsesURLCitationAnnotation`; all other annotation types (including `file_citation`) return `nil` with no error (`translator_responses.go:490-502`). The annotation is then silently absent from the canonical output. |
| `ResponsesRefusalPart` | 🔀 | Mapped to `*v1.OutputTextPart{Text: v.Refusal}` per canonical rule 9 (`translator_responses.go:480-483`). The refusal-specific semantic is preserved only if `finish_reason` is also set to `refusal` — which happens in `responsesFinishReasonToCanonical` (there is no Responses `refusal` finish reason, so this only applies when the canonical response is serialized back). |

---

## Response: Responses → canonical (`ParseResponse`)

| Responses field | Status | Notes + file:line |
|---|---|---|
| `id`, `object`, `created_at`, `model`, `status` | ✅ | `translator_responses.go:604-609` |
| `finish_reason` | ✅ | `responsesFinishReasonToCanonical`, `translator_responses.go:611` |
| `output` items | ✅ | Loop via `responsesItemToCanonical`, `translator_responses.go:613-618` |
| `usage.input_tokens` (minus cached) | ✅ | `translator_responses.go:678-680` — correctly subtracts `cached_tokens` for non-overlapping dimensions |
| `usage.output_tokens` | ✅ | `translator_responses.go:681-683` |
| `usage.input_tokens_details.cached_tokens` → `cache_read` | ✅ | `translator_responses.go:684-686` |
| `usage.output_tokens_details.reasoning_tokens` → `reasoning` | ✅ | `translator_responses.go:687-689` |
| `usage.output_tokens_details.audio_tokens` | ⛔ | **Silently dropped.** `ResponsesOutputDeets` has only `ReasoningTokens` (`responses_response.go:94-96`); audio token fields not modeled. |
| `usage.input_tokens_details.audio_tokens` | ⛔ | Same — not modeled in `ResponsesInputDeets` (`responses_response.go:89-91`). |
| `usage` → `cache_creation` | ⛔ | Not produced by the Responses API (OpenAI caches automatically, no creation event). Canonical key `cache_creation` is never populated. |
| `error` | ✅ | `translator_responses.go:622-624` |
| `incomplete_details` | ✅ | `translator_responses.go:625-627` |
| Request-echo fields (`instructions`, `temperature`, `top_p`, `tools`, `tool_choice`, `parallel_tool_calls`, `metadata`) | ✅ stripped | Correctly ignored — `UnmarshalResponsesResponse` (`responses_serialize.go:16-53`) does not decode them; they are not part of canonical. |
| `ResponsesReasoning.EncryptedContent` in output | ⛔ | **Silently dropped.** `responsesItemToCanonical` for `*ResponsesReasoning` explicitly notes the drop but does NOT store in `ProviderData` (`translator_responses.go:439-453`). Same-vendor round-trip of reasoning items will lose `encrypted_content`. |
| `ResponsesFileCitationAnnotation` on output parts | ⛔ | **Silently dropped.** `responsesAnnotationToCanonical` returns `nil` for non-url-citation annotations (`translator_responses.go:490-502`); the annotation is excluded from the canonical `OutputTextPart.Annotations` array with no error. |
| `ResponsesOutputTextPart.Logprobs` | ⛔ | **Silently dropped.** `responsesPartToCanonical` for `*ResponsesOutputTextPart` constructs `v1.OutputTextPart` without logprobs — canonical has no logprobs field (`translator_responses.go:461-468`). |
| `response.failed` status | ⚠️ | `responsesFinishReasonToCanonical` default case maps unknown finish reason to `v1.FinishReasonStop` (`translator_responses.go:644-646`). A `failed` response with a vendor-specific finish reason like `"max_completion_tokens"` will be silently mapped to `stop`. |

---

## Response: canonical → Responses (`SerializeResponse`)

| Canonical field | Status | Notes + file:line |
|---|---|---|
| `id`, `object`, `created_at`, `model`, `status` | ✅ | `translator_responses.go:118-124` |
| `created_at` fallback | 🔒 | `time.Now().Unix()` when `resp.CreatedAt == 0` (`translator_responses.go:125-127`). |
| `finish_reason` | ✅ | `translator_responses.go:130` |
| `FinishReasonRefusal` → Responses | 🔀 | Mapped to `ResponsesFinishReasonStop` (`translator_responses.go:660-663`). Responses API has no `refusal` finish reason; the refusal text is in the message content (canonical rule 9 compliant), but a downstream Responses consumer cannot distinguish a refusal stop from a normal stop. |
| `output` items | ✅ | `translator_responses.go:133-138` |
| `usage` | ✅ | `translator_responses.go:140-143` via `canonicalUsageToResponses` |
| `usage.cache_creation` | ⛔ | `canonicalUsageToResponses` does not read `t["cache_creation"]` (`translator_responses.go:699-716`). If canonical carries a `cache_creation` count (e.g., from an Anthropic upstream via cross-shape), the field is dropped. |
| `error` | ✅ | `translator_responses.go:145-149` |
| `incomplete_details` | ✅ | `translator_responses.go:150-153` |
| `Extensions` | ⛔ | **Silently dropped.** `resp.Extensions` is never read in `SerializeResponse` (`translator_responses.go:117-167`). |
| Request echo | ✅ | `translator_responses.go:158-164` via `ResponsesEchoRequest` |
| `Message.ProviderData` in output | ⛔ | `responsesItemFromCanonical` for `*v1.Message` doesn't copy `ProviderData` into any wire field (`translator_responses.go:506-520`). |
| `Reasoning.ProviderData` → `encrypted_content` | ⛔ | **Silently dropped.** `responsesItemFromCanonical` for `*v1.Reasoning` constructs `ResponsesReasoning` without reading `ProviderData` (`translator_responses.go:544-556`). A same-vendor round-trip via canonical loses the encrypted signature. |
| `Reasoning.Content` (raw reasoning text) | ⛔ | **Silently dropped.** `responsesItemFromCanonical` only copies `Summary` array, not `v1.Reasoning.Content` (`translator_responses.go:544-556`). |
| `ResponsesOutputTextPart.Logprobs` | 🔒 | Always emitted as `[]` (empty array) per spec (`responses_parts.go:110-112`). No canonical source for logprobs. |

---

## Streaming

### To canonical (`NewToCanonicalStream`)

| Responses event | → Canonical event | Notes + file:line |
|---|---|---|
| `response.created` | → `generation.created` | `translator_responses.go:738-755`. Snapshot fields beyond `id` and `model` (status, output, instructions, tools, etc.) are dropped — only id+model extracted. |
| `response.in_progress` | → **dropped** | `translator_responses.go:757-759`. No canonical equivalent. |
| `response.output_item.added` | → `item.started` | `translator_responses.go:760-790`. Item type and id extracted. **`Name` field of `ItemStartedEvent` is NOT populated for FunctionCall items** — the canonical spec says `Name` is for FunctionCall items so downstream serializers don't have to wait; this adapter leaves it empty. |
| `response.content_part.added` | → **dropped** | Falls to default at `translator_responses.go:889`. No canonical equivalent (canonical has no per-part-start event). |
| `response.output_text.delta` | → `item.delta` (kind=text) | `translator_responses.go:792-803` |
| `response.output_text.done` | → **dropped** | Falls to default. Redundant with `item.completed`; intentional. |
| `response.content_part.done` | → **dropped** | Falls to default. Intentional. |
| `response.output_item.done` | → `item.completed` | `translator_responses.go:831-859`. Full item unmarshaled and translated. |
| `response.function_call_arguments.delta` | → `item.delta` (kind=arguments) | `translator_responses.go:805-816` |
| `response.function_call_arguments.done` | → **dropped** | Falls to default. Redundant with `item.completed`. |
| `response.reasoning_text.delta` | → `item.delta` (kind=reasoning) | `translator_responses.go:818-829` |
| `response.reasoning_text.done` | → **dropped** | Falls to default. Redundant with `item.completed`. |
| `response.refusal.delta` | → **dropped** | **SILENT DATA LOSS.** `ResponsesEventRefusalDelta` is not in the switch (`translator_responses.go:738-892`). Refusal streaming content is entirely lost mid-stream. |
| `response.refusal.done` | → **dropped** | Same — not handled. Refusal text does not appear in any canonical stream event. |
| `response.completed` | → `generation.completed` | `translator_responses.go:861-876` |
| `response.incomplete` | → `generation.completed` | Same case branch; status=incomplete carried. |
| `response.failed` | → **dropped** | **SILENT.** Not in switch; falls to default. A failed response emits nothing to canonical stream. |
| `error` | → `error` | `translator_responses.go:878-888` |

**Information lost in collapse (all silently):**
- `content_index` on text/refusal events (canonical has one delta kind per item, no sub-part index)
- Streaming logprobs (no canonical home)
- `response.in_progress` lifecycle signal
- `response.output_text.done` final-text confirmation
- Partial function call `call_id` in delta events (canonical arguments delta carries no call_id)
- All built-in tool streaming events (web_search_call, file_search_call, computer_call, etc.) — these event types are not modeled in `responses_events.go` at all; if the upstream emits them they will be silently dropped via the default branch

### From canonical (`NewFromCanonicalStream`)

| Canonical event | → Responses events | Notes + file:line |
|---|---|---|
| `generation.created` | → `response.created` + `response.in_progress` | `translator_responses.go:949-969`. `created` timestamp set to `time.Now()` — does not use canonical event's data; `GenerationCreatedEvent` carries no timestamp. |
| `item.started` (message) | → `output_item.added` + `content_part.added` | `translator_responses.go:982-997`. Role is **hardcoded to `assistant`** regardless of item — `translator_responses.go:984`. |
| `item.started` (function_call) | → `output_item.added` | `translator_responses.go:999-1005`. **`Name` and `CallID` fields of the emitted stub are empty** because `ItemStartedEvent.Name` (populated by the to-canonical direction) is not stored into `responsesStreamItem.name`/`.callID` — `translator_responses.go:976-979`. Downstream Responses consumers relying on `output_item.added` for function name will see empty. |
| `item.started` (reasoning) | → `output_item.added` | `translator_responses.go:1007-1013` |
| `item.started` (function_call_output) | → **nothing emitted** | No case for `v1.ItemTypeFunctionCallOutput`; falls through silently. |
| `item.delta` (text) | → `output_text.delta` | `translator_responses.go:1028-1036`. Accumulates in `st.textBuf`. |
| `item.delta` (arguments) | → `function_call_arguments.delta` | `translator_responses.go:1037-1047`. `call_id` in the emitted event is `st.callID` — which is always empty (never populated, see above). |
| `item.delta` (reasoning) | → `reasoning_text.delta` | `translator_responses.go:1048-1058`. Note: delta is accumulated into `st.textBuf` — same buffer as text delta (`translator_responses.go:1049`). If a reasoning item somehow also has text content they would interleave into the same buffer. |
| `item.completed` (message) | → `output_text.done` + `content_part.done` + `output_item.done` | `translator_responses.go:1079-1106`. Reconstructed from buffer. **Annotations on the completed item are not reconstructed** — final message carries `text` only. |
| `item.completed` (function_call) | → `function_call_arguments.done` + `output_item.done` | `translator_responses.go:1108-1151`. `call_id` in `function_call_arguments.done` is empty (see above). The "best-effort enrichment" from `evItemRaw.Item` at `translator_responses.go:1133-1145` attempts to recover call_id/name from the canonical `item.completed` payload, but `ItemCompletedEvent.Item` serializes as a `v1.FunctionCall` — the two-phase parse at `translator_responses.go:1065-1072` discards the `Item` field entirely (only `item_id` and `index` extracted), so the enrichment always uses the empty-struct path. **call_id is always empty in `function_call_arguments.done`**. |
| `item.completed` (reasoning) | → `reasoning_text.done` + `output_item.done` | `translator_responses.go:1153-1171`. Reconstructed summary is `[]ResponsesSummaryText{{Text: st.textBuf}}` — single element with the full accumulated text, regardless of how many summary segments the original reasoning had. |
| `generation.completed` | → `response.completed` or `response.incomplete` | `translator_responses.go:1174-1200`. Correctly distinguishes `status == incomplete`. |
| `error` | → `error` | `translator_responses.go:1202-1208` |

---

## ⚠️ Silently dropped (no error, no log)

1. **`Reasoning.EncryptedContent`** — parsed from Responses wire (`responses_items.go:150`), comment says it will be stored in `ProviderData` for round-trip (`translator_responses.go:447`), but the store never happens. Canonical `Reasoning.ProviderData` stays nil. Same-vendor reasoning round-trips lose the OpenAI signature required to feed reasoning items back. — `translator_responses.go:439-453`

2. **`FileCitationAnnotation`** — `responsesAnnotationToCanonical` returns `nil` for `*ResponsesFileCitationAnnotation` (and any other non-url-citation type); the annotation is excluded from `OutputTextPart.Annotations` silently. — `translator_responses.go:490-502`

3. **`Response.Extensions`** — `resp.Extensions` never read in `SerializeResponse`. — `translator_responses.go:117-167`

4. **`Request.Extensions`** — `req.Extensions` never read in `SerializeRequest`. — `translator_responses.go:44-102`

5. **`logprobs` / `top_logprobs`** — parsed into `ResponsesRequest` struct (`responses_parse.go:102-104`) then never forwarded to canonical; no error returned. — `translator_responses.go:248-264`

6. **`store: false`** — `responsesRejectStatefulFields` only rejects `store == true`; `store: false` is silently accepted and discarded. — `translator_responses.go:190-192`

7. **`response.refusal.delta` / `response.refusal.done`** — stream events not in the `case` switch; fall to the "unknown/unhandled" default comment. Refusal streaming content is silently discarded mid-stream. A Responses client streaming a refusal gets no text deltas. — `translator_responses.go:889-892`

8. **`response.failed`** — stream event not in the `case` switch; falls to default. A streaming failed response produces no canonical event. — `translator_responses.go:889-892`

9. **`MediaType` on `FilePart`** — `responsesPartToCanonical` for `*ResponsesFilePart` omits `MediaType` (`translator_responses.go:472-479`); `v1.FilePart.MediaType` stays empty.

10. **`Reasoning.Content`** (raw text) — `responsesItemFromCanonical` only copies the `Summary` array, not `v1.Reasoning.Content`. — `translator_responses.go:544-556`

11. **ServerTool / MCPTool in tool definitions** — `canonicalToResponsesRequest` silently skips non-FunctionTool definitions with `continue`. No error, no log. — `translator_responses.go:368-371`

---

## 🔒 Hardcoded / defaulted

1. **`created_at` fallback to `time.Now()`** when `resp.CreatedAt == 0` in `SerializeResponse`. — `translator_responses.go:125-127`

2. **`tool_choice` defaults to `"auto"`** in `ResponsesResponse.MarshalJSON` when `ToolChoiceRaw` is empty. — `responses_response.go:59-61`

3. **`parallel_tool_calls` defaults to `true`** in `ResponsesEchoRequest` when request has no explicit value. — `responses_response.go:128-130`

4. **`logprobs` always `[]`** in `ResponsesOutputTextPart.MarshalJSON` — emits empty array, never populated from canonical. — `responses_parts.go:110-112`

5. **Role hardcoded to `assistant`** in `item.started` → `output_item.added` for message items in `NewFromCanonicalStream`. — `translator_responses.go:984`

6. **`created` timestamp in from-canonical stream** uses `time.Now().Unix()` at `generation.created` time, not the canonical event's timestamp (which has no timestamp field). — `translator_responses.go:961`

7. **Reasoning summary reconstruction** in `item.completed` for reasoning items: `[]ResponsesSummaryText{{Text: st.textBuf}}` — single element regardless of original multi-segment structure. — `translator_responses.go:1164`

8. **Function parameters default to `{}`** when `nil` in tool definitions. — `translator_responses.go:274-276`

---

## ⛔ Unsupported (explicit error)

1. **`previous_response_id`** — `responses_unsupported_canonical` error. `translator_responses.go:187-189`
2. **`store: true`** — `responses_unsupported_canonical` error. `translator_responses.go:190-192`
3. **`conversation`** — `responses_unsupported_canonical` error. `translator_responses.go:193-195`
4. **`background: true`** — `responses_unsupported_canonical` error. `translator_responses.go:196-198`
5. **`truncation`** — `responses_unsupported_canonical` error. `translator_responses.go:199-201`
6. **`service_tier`** — `responses_unsupported_canonical` error. `translator_responses.go:202-204`
7. **`safety_identifier`** — `responses_unsupported_canonical` error. `translator_responses.go:205-207`
8. **`prompt_cache_key`** — `responses_unsupported_canonical` error. `translator_responses.go:208-210`
9. **`context_management`** — `responses_unsupported_canonical` error. `translator_responses.go:211-214`
10. **`include`** — `responses_unsupported_canonical` error. `translator_responses.go:215-217`
11. **Built-in tool types** (web_search_preview, file_search, computer_use_preview, etc.) — hard parse error in `responsesUnmarshalTool`. `responses_tools.go:86`
12. **Unsupported item types** — hard parse error in `responsesUnmarshalItem`. `responses_items.go:207-209`

---

## Responses features with NO canonical representation

These are Responses API capabilities the canonical layer structurally cannot express. Any
cross-shape route that involves them requires either explicit rejection or silent semantic loss.

| Feature | Gap |
|---|---|
| **Server-side conversation state** (`previous_response_id`, `store`, `conversation`) | Canonical is stateless — no session/thread concept. Requires explicit rejection (done). |
| **Background processing** (`background: true`) | No canonical async-response-later model. Rejected. |
| **Context window management** (`truncation`, `context_management`) | No canonical token-budget management directives. Rejected. |
| **Service tier / routing hints** (`service_tier`) | No canonical upstream-routing hint. Rejected. |
| **Safety labeling** (`safety_identifier`) | No canonical safety context. Rejected. |
| **Explicit prompt caching key** (`prompt_cache_key`) | Canonical uses `CacheConfig` intent flags, not explicit keys. Rejected. |
| **Output inclusion control** (`include[]`) | No canonical way to request e.g. `file_search_call.results` inclusion. Rejected. |
| **Logprobs** (`logprobs`, `top_logprobs`) | No canonical logprobs field. Parsed then silently discarded. |
| **Built-in tools** (web_search_preview, file_search, computer_use_preview, code_interpreter, mcp) | Only `function` type maps to canonical `FunctionTool`. Built-ins hard-error at parse. |
| **Streaming refusal events** (`response.refusal.delta/done`) | Canonical has no refusal-specific stream events (refusal is text + finish_reason per rule 9). Streaming refusal content is silently dropped. |
| **Reasoning summary configuration** (`reasoning.summary: "concise"/"detailed"`) | `ResponsesReasoningConfig` lacks this field entirely. |
| **Reasoning `encrypted_content`** | No canonical field. Parsed and then silently discarded despite comment promising round-trip storage. |
| **`response.failed` stream event** | Not handled; no canonical equivalent (distinct from `generation.completed` with `status=failed`). |
| **File citation annotations** (`file_citation`) | `responsesAnnotationToCanonical` returns nil. No canonical `FileCitationAnnotation` type. |
| **Audio token metering** (`audio_tokens` in input/output details) | `ResponsesInputDeets`/`ResponsesOutputDeets` structs don't model audio fields. |
| **Multi-model routing** (`model: [...]`) | Canonical `ModelRefs` accepts it; runtime rejects with `ErrMultiplexNotImplemented`. `SerializeRequest` silently uses only first model. |

---

## Round-trip fidelity

### canonical → Responses → canonical

| Element | Survives? |
|---|---|
| Text content, instructions, model, user, metadata | ✅ |
| Sampling (temp, top_p, max_tokens, stop) | ✅ |
| Seed, frequency_penalty, presence_penalty | ✅ in canonical; ⛔ dropped to Responses (no wire field), lost on return |
| TopK | ✅ into canonical; ⚠️ dropped on serialize back (comment at `translator_responses.go:352`) |
| Reasoning effort | ✅ |
| Reasoning summary, budget_tokens | ⛔ dropped on serialize |
| Tools (function) | ✅ |
| Tools (server/mcp) | ⛔ silently skipped on serialize |
| Output format | ✅ |
| Output verbosity | ⛔ dropped on serialize |
| FunctionCall items | ✅ |
| Reasoning items (summary) | ✅ |
| Reasoning items (ProviderData / encrypted_content) | ⛔ dropped |
| Message ProviderData | ⛔ dropped |
| CacheConfig / ItemCacheConfig | ⛔ dropped (intentional, OpenAI auto-caches) |
| Extensions (request/response) | ⛔ dropped |

### Responses → canonical → Responses

| Element | Survives? |
|---|---|
| Core fields | ✅ |
| URL citation annotations | ✅ |
| File citation annotations | ⛔ dropped (nil from `responsesAnnotationToCanonical`) |
| Logprobs | ⛔ dropped |
| `encrypted_content` on Reasoning | ⛔ dropped despite comment |
| Output text logprobs | ⛔ dropped; re-serialized as `[]` |
| FilePart MediaType | ⛔ dropped |

---

## Recommendations (prioritized)

**P0 — Data loss with production consequences:**

1. **Fix `encrypted_content` round-trip** (`translator_responses.go:439-453`). The comment says it will be stored in `ProviderData` but the code does nothing. OpenAI requires `encrypted_content` to be fed back verbatim when using reasoning items across turns. Without this, any stateless routing through canonical breaks multi-turn reasoning. Implement: store `v.EncryptedContent` in `json.RawMessage` into `r.ProviderData` on parse; read `ProviderData` back in `responsesItemFromCanonical` to populate `ResponsesReasoning.EncryptedContent`.

2. **Handle `response.refusal.delta` / `response.refusal.done` in `NewToCanonicalStream`** (`translator_responses.go:889`). Currently these fall to the default-drop branch. A streaming refusal sends no text to the canonical consumer. Add cases that accumulate refusal text as `item.delta` (kind=text) events, mapping to `OutputTextPart` per canonical rule 9. The `response.refusal.done` should participate in `item.completed`.

3. **Handle `response.failed` stream event** (`translator_responses.go:889`). Parse as `ResponsesFailedEvent`, translate to `generation.completed` with `status=failed`. Currently a streaming failed response produces no terminal canonical event, leaving stream consumers hanging.

4. **Populate `Name` and `CallID` in `responsesStreamItem` from `ItemStartedEvent`** (`translator_responses.go:976-979`). `v1.ItemStartedEvent.Name` exists for this exact purpose but is not stored. This causes every `function_call_arguments.delta` and `function_call_arguments.done` in the from-canonical direction to emit `call_id: ""` and `name: ""`, breaking function-call streaming for any Responses consumer.

5. **`file_citation` annotation** (`translator_responses.go:490-502`). `responsesAnnotationToCanonical` silently drops `*ResponsesFileCitationAnnotation`. Add a canonical `v1.Annotation` type for file citations or fall through to `*v1.RawAnnotation` to preserve the data.

**P1 — Semantic loss that violates consumer contracts:**

6. **Pass `Response.Extensions` through `SerializeResponse`** (`translator_responses.go:117-167`). Extensions from cross-shape upstreams (e.g., Anthropic safety fields) are silently dropped on the Responses output. Add extensions to `ResponsesResponse` and serialize them.

7. **`logprobs` / `top_logprobs`** (`responses_parse.go:102-104`): emit an explicit `responses_unsupported_canonical` error on parse rather than silently discarding. Callers who set `logprobs: true` expect logprobs in the response; silently dropping makes the response incorrect without any signal.

8. **`FilePart.MediaType`** (`translator_responses.go:472-479`): one-line fix to copy `v.Filename` is already there; add `MediaType: v.MediaType` (note: `ResponsesFilePart` has no `MediaType` field today — may need to be added to match Responses API spec).

**P2 — Documentation / minor correctness:**

9. **`FinishReasonRefusal` → Responses**: the mapped `stop` is correct per spec, but callers cannot distinguish refusal from normal stop. Document this narrowing explicitly.

10. **Multi-model `Model[0]` silently truncated** (`translator_responses.go:87`): add an explicit error if `len(req.Model) > 1` before serializing, consistent with `ErrMultiplexNotImplemented`.
