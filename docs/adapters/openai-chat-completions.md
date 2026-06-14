# OpenAI Chat Completions — Canonical Fidelity Audit

> Audited 2026-05-25 against `sdk/adapters/openai/translator_cc.go`. Critical, honest map of what survives canonical translation.

## Verdict

Overall fidelity is good for the common text+tools case but has several material silent drops. The single most dangerous gap is that `Annotations` on `ChatResponseMessage` (URL citations from OpenAI's web-search tool) are parsed by the wire type but never transferred to canonical output, so they vanish silently on every response that uses web search. A second structural hazard is that `NewFromCanonicalStream()` returns `nil` (hard panics any caller that dereferences it), and there is no guard at the call site documented here. The `Extensions` envelope is respected at zero (never read, never written), and per-item `CacheConfig`/`ProviderData` round-tripping is lost in both directions.

---

## Request: canonical → OpenAI (SerializeRequest)

| Canonical element | Status | Notes + file:line |
|---|---|---|
| `Message.Role` user/assistant/system | ✅ mapped | `canonicalMessageToCC` translator_cc.go:611 |
| `Message.Role` developer | 🔀 transformed | Emitted as `"system"` role — translator_cc.go:614 |
| `Message.Content` TextPart | ✅ mapped | `canonicalPartsToCC` translator_cc.go:633 |
| `Message.Content` OutputTextPart | ✅ mapped | translator_cc.go:648 |
| `Message.Content` ImagePart | ✅ mapped | translator_cc.go:679 |
| `Message.Content` FilePart | ✅ mapped | translator_cc.go:685 (FileURL silently dropped — only FileID/FileData/Filename serialized) |
| `Message.CacheConfig` (per-item anchor) | ⚠️ dropped | No code touches `m.CacheConfig` in `canonicalMessageToCC`; per canonical design OpenAI ignores it, but the field is not even read — translator_cc.go:611–629 |
| `Message.ProviderData` | ⚠️ dropped | Never read in `canonicalMessageToCC` — translator_cc.go:611 |
| `Message.ID` / `Message.Status` (input) | ⚠️ dropped | Only output Messages carry these; fine for input but worth noting |
| `FunctionCall.CallID` | ✅ mapped | translator_cc.go:576 |
| `FunctionCall.Name` / `.Arguments` | ✅ mapped | translator_cc.go:578–581 |
| `FunctionCall.ProviderData` | ⚠️ dropped | Not read in `canonicalItemsToCC` FunctionCall branch — translator_cc.go:573 |
| `FunctionCallOutput.CallID` + `.Output` | ✅ mapped | translator_cc.go:594–600 |
| `FunctionCallOutput.Content` ([]Part) | 🔀 transformed | Only `*v1.TextPart` concatenated to string; non-text parts silently dropped — translator_cc.go:712–719 |
| `Reasoning` item (input) | ⚠️ dropped | `case *v1.Reasoning:` comment says "Drop" — translator_cc.go:603 |
| `Instructions` | ✅ mapped | Becomes `system` message — translator_cc.go:559 |
| `ModelConfig[model].Sampling.Temperature` | ✅ mapped | translator_cc.go:233 |
| `ModelConfig[model].Sampling.TopP` | ✅ mapped | translator_cc.go:234 |
| `ModelConfig[model].Sampling.TopK` | ⚠️ dropped | CC has no `top_k`; not emitted, not errored — translator_cc.go:229–247 (field absent) |
| `ModelConfig[model].Sampling.MaxTokens` | ✅ mapped | → `max_tokens` — translator_cc.go:235 |
| `ModelConfig[model].Sampling.Stop` | ✅ mapped | translator_cc.go:243–246 |
| `ModelConfig[model].Sampling.Seed` | ✅ mapped | translator_cc.go:237–240 |
| `ModelConfig[model].Sampling.FrequencyPenalty` | ✅ mapped | translator_cc.go:236 |
| `ModelConfig[model].Sampling.PresencePenalty` | ✅ mapped | translator_cc.go:237 |
| `ModelConfig[model].Reasoning.Effort` | ✅ mapped | → `reasoning_effort` — translator_cc.go:249 |
| `ModelConfig[model].Reasoning.Summary` | ⚠️ dropped | `ReasoningConfig.Summary` never read — translator_cc.go:248–250 |
| `ModelConfig[model].Reasoning.BudgetTokens` | ⚠️ dropped | `ReasoningConfig.BudgetTokens` never read — translator_cc.go:248–250 |
| `Request.Tools.Definitions` FunctionTool | ✅ mapped | translator_cc.go:260–278 |
| `Request.Tools.Definitions` ServerTool | ⛔ error | Returns `fmt.Errorf("unsupported tool type %T")` — translator_cc.go:263 |
| `Request.Tools.Definitions` MCPTool | ⛔ error | Same path as ServerTool — translator_cc.go:263 |
| `Request.Tools.Choice` | ✅ mapped | translator_cc.go:280–284 |
| `Request.Tools.Parallel` | ✅ mapped | → `parallel_tool_calls` — translator_cc.go:279 |
| `ModelConfig[model].Output.Format` text/json_object/json_schema | ✅ mapped | `ccFormatToResponseFormat` translator_cc.go:846 |
| `ModelConfig[model].Output.Verbosity` | ⚠️ dropped | `OutputConfig.Verbosity` never read — translator_cc.go:251–257 |
| `CacheConfig` (request-level) | ⚠️ dropped | Intentional per cache.go comment; OpenAI auto-caches. No code reads `req.CacheConfig` — translator_cc.go:217–302 |
| `OutputMode` stream/sync | ✅ mapped | → `stream` bool — translator_cc.go:289–292 |
| `User` | ✅ mapped | translator_cc.go:224 |
| `Metadata` | ✅ mapped | translator_cc.go:225 |
| `Extensions` | ⚠️ dropped | Never read in `SerializeRequest` — translator_cc.go:217–302 |

---

## Request: OpenAI → canonical (ParseRequest)

| OpenAI field | Status | Notes + file:line |
|---|---|---|
| `model` | ✅ mapped | → `req.Model` — translator_cc.go:47 |
| `messages[].role` system (first) | ✅ mapped | → `req.Instructions` — translator_cc.go:175 |
| `messages[].role` system (subsequent) | 🔀 transformed | → `developer` role Message — translator_cc.go:179–183 |
| `messages[].role` developer | ✅ mapped | translator_cc.go:185–190 |
| `messages[].role` user (text) | ✅ mapped | translator_cc.go:192–196 |
| `messages[].role` user (image_url) | ✅ mapped | translator_cc.go:493–495 |
| `messages[].role` user (file) | ✅ mapped | `file_id`, `file_data`, `filename` — translator_cc.go:498–510 |
| `messages[].role` user (input_audio) | ⚠️ dropped | `ContentPart.Audio` field is parsed by the type struct (types.go:63) but `ccContentToParts` has no `case "input_audio"` — translator_cc.go:488–513 |
| `messages[].role` assistant (text) | ✅ mapped | translator_cc.go:524–538 |
| `messages[].role` assistant (refusal) | 🔀 transformed | Refusal text appended as second `OutputTextPart`; `finish_reason=refusal` is NOT set on the request-side parse (only on response side). Round-tripping works but the semantic distinction is lost at parse time — translator_cc.go:537–539 |
| `messages[].tool_calls` (assistant) | ✅ mapped | → `FunctionCall` items — translator_cc.go:544–552 |
| `messages[].role` tool | ✅ mapped | → `FunctionCallOutput` — translator_cc.go:203–208 |
| `messages[].name` | ⚠️ dropped | `ChatMessage.Name` is parsed by the struct (types.go:51) but never copied into any canonical field — translator_cc.go:520 |
| `temperature` | ✅ mapped | translator_cc.go:58–61 |
| `top_p` | ✅ mapped | translator_cc.go:62–65 |
| `max_tokens` | ✅ mapped | translator_cc.go:66–69 |
| `max_completion_tokens` | ✅ mapped | → same `sampling.MaxTokens` (last one wins if both present) — translator_cc.go:70–73 |
| `frequency_penalty` | ✅ mapped | translator_cc.go:74–77 |
| `presence_penalty` | ✅ mapped | translator_cc.go:78–81 |
| `seed` | ✅ mapped | translator_cc.go:82–86 |
| `stop` (string or array) | ✅ mapped | translator_cc.go:87–100 |
| `reasoning_effort` | ✅ mapped | translator_cc.go:132–134 |
| `response_format` | ✅ mapped | translator_cc.go:137–155 |
| `tools[].function` | ✅ mapped | translator_cc.go:108–128 |
| `tool_choice` | ✅ mapped | translator_cc.go:122–127 |
| `parallel_tool_calls` | ✅ mapped | translator_cc.go:121 |
| `stream` | ✅ mapped | → `req.OutputMode` — translator_cc.go:158–162 |
| `user` | ✅ mapped | translator_cc.go:49 |
| `metadata` | ✅ mapped | translator_cc.go:50–52 |
| `n` (multi-completion) | ⚠️ dropped | Parsed in `FullChatRequest` struct (types.go:26) but never read in `ParseRequest` — translator_cc.go:37–214 |
| `logit_bias` | ⚠️ dropped | Parsed in struct (types.go:27) but never read — translator_cc.go:37–214 |
| `logprobs` / `top_logprobs` | ⚠️ dropped | Parsed in struct (types.go:29–30) but never read — translator_cc.go:37–214 |
| `service_tier` | ⚠️ dropped | types.go:35; never read |
| `store` | ⚠️ dropped | types.go:37; never read |
| `stream_options` | ⚠️ dropped | Parsed (types.go:34) but never copied — translator_cc.go:37–214 |
| `top_k` | — | Not a standard CC field; correctly absent |

---

## Response: OpenAI → canonical (ParseResponse)

| OpenAI response field | Status | Notes + file:line |
|---|---|---|
| `id` | ✅ mapped | translator_cc.go:311 |
| `object` | 🔒 hardcoded | Always set to `"response"` not the vendor value — translator_cc.go:313 |
| `created` | ✅ mapped | translator_cc.go:314; fallback to `time.Now()` if zero — translator_cc.go:317 |
| `model` | ✅ mapped | translator_cc.go:315 |
| `system_fingerprint` | ⚠️ dropped | Parsed in struct (types.go:130) but never propagated to canonical Extensions — translator_cc.go:305–336 |
| `service_tier` | ⚠️ dropped | types.go:131; same |
| `choices[0].finish_reason` stop/length/tool_calls/content_filter | ✅ mapped | `ccFinishReasonToCanonical` translator_cc.go:726–739 |
| `choices[0].finish_reason` "refusal" | ⚠️ dropped | `ccFinishReasonToCanonical` default branch maps unknown reasons to `StatusCompleted/stop` — translator_cc.go:736–738. OpenAI emits `"stop"` for refusals anyway (refusal is carried in `message.refusal`), so in practice the refusal finish_reason path is via the `message.refusal` field check, not `finish_reason`. But the mapping is still incomplete. |
| `choices[0].message.content` | ✅ mapped | → `OutputTextPart` — translator_cc.go:762–764 |
| `choices[0].message.refusal` | 🔀 transformed | Appended as second `OutputTextPart` on same Message item; no separate item type; `FinishReason` set to `refusal` via `ccFinishReasonToCanonical` only if CC also returns `finish_reason="refusal"` (it usually returns `"stop"` when refusal is present) — translator_cc.go:765–769 |
| `choices[0].message.tool_calls` | ✅ mapped | → `FunctionCall` items — translator_cc.go:774–781 |
| `choices[0].message.annotations` | ⚠️ dropped | `ChatResponseMessage.Annotations` parsed by struct (types.go:162) but `ccChoiceToCanonicalOutput` never reads `msg.Annotations` — translator_cc.go:742–784 |
| `choices[0].logprobs` | ⚠️ dropped | Parsed in struct (types.go:152) but never propagated — translator_cc.go:742 |
| `usage.prompt_tokens` | ✅ mapped | → `input` (minus cached) — translator_cc.go:802–805 |
| `usage.completion_tokens` | ✅ mapped | → `output` — translator_cc.go:806–808 |
| `usage.prompt_tokens_details.cached_tokens` | ✅ mapped | → `cache_read` — translator_cc.go:809–811 |
| `usage.prompt_tokens_details.audio_tokens` | ⚠️ dropped | `ccUsageToCanonical` does not read `PromptDetails.AudioTokens`; `ExtractTokens` in tokens.go:64 does map it, but `ccUsageToCanonical` (used by ParseResponse and stream) does not — translator_cc.go:794–819 |
| `usage.completion_tokens_details.reasoning_tokens` | ✅ mapped | → `reasoning` — translator_cc.go:812–814 |
| `usage.completion_tokens_details.audio_tokens` | ⚠️ dropped | `ccUsageToCanonical` does not read `CompletionDetails.AudioTokens` — translator_cc.go:794–819 |
| `usage.completion_tokens_details.accepted_prediction_tokens` | ⚠️ dropped | Not read in `ccUsageToCanonical` — translator_cc.go:794–819 |
| `usage.completion_tokens_details.rejected_prediction_tokens` | ⚠️ dropped | Not read in `ccUsageToCanonical` — translator_cc.go:794–819 |
| `error` | ⚠️ dropped | `ChatResponse` struct has no `Error` field; non-2xx error bodies are not handled by `ParseResponse` — translator_cc.go:305–336 |
| `incomplete_details` | ✅ mapped | Via `ccFinishReasonToCanonical` — translator_cc.go:332, 727–739 |
| Only `choices[0]` consumed | ⚠️ dropped | `n>1` responses silently discard choices[1..n] — translator_cc.go:331 |

---

## Response: canonical → OpenAI (SerializeResponse)

| Canonical field | Status | Notes + file:line |
|---|---|---|
| `id` | ✅ mapped | translator_cc.go:342 |
| `object` | 🔒 hardcoded | Always `"chat.completion"` — translator_cc.go:343 |
| `created_at` | ✅ mapped | translator_cc.go:344 |
| `model` | ✅ mapped | translator_cc.go:345 |
| `status` | ⚠️ dropped | Not surfaced in CC wire shape; fine as CC has no status field |
| `finish_reason` stop/length/tool_calls/content_filter | ✅ mapped | translator_cc.go:358–370 |
| `finish_reason` refusal | 🔀 transformed | Mapped to CC `"stop"` + sets `message.refusal` — translator_cc.go:366, 404–408 |
| `output` Message (text) | ✅ mapped | translator_cc.go:376–390 |
| `output` Message (OutputTextPart annotations) | ⚠️ dropped | `OutputTextPart.Annotations` are never serialized to CC `message.annotations` — translator_cc.go:378–385 |
| `output` FunctionCall | ✅ mapped | → `tool_calls` — translator_cc.go:391–399 |
| `output` Reasoning | ⚠️ dropped | Explicit comment "drop" — translator_cc.go:401–402 |
| `usage` input/output/cache_read/reasoning | ✅ mapped | `canonicalUsageToCC` translator_cc.go:824–843 |
| `usage` audio_input/audio_output | ⚠️ dropped | `canonicalUsageToCC` does not emit audio fields — translator_cc.go:824–843 |
| `usage` accepted/rejected_prediction | ⚠️ dropped | Same — translator_cc.go:824–843 |
| `error` | ⚠️ dropped | `v1.Response.Error` never serialized to `ChatResponse` — translator_cc.go:341–423 |
| `incomplete_details` | ⚠️ dropped | Field exists in `v1.Response` but `ChatResponse` struct has no matching field — types.go:126–134 |
| `extensions` | ⚠️ dropped | Never read — translator_cc.go:341 |
| Multiple `output` Message items | 🔀 transformed | All text parts across all Message items concatenated into one `choices[0].message.content` string — translator_cc.go:375–412 |

---

## Streaming

### To canonical (NewToCanonicalStream)

The `ccToCanonicalStream.translate` method handles CC SSE → canonical SSE.

| Aspect | Status | Notes + file:line |
|---|---|---|
| `generation.created` | ✅ emitted | On first chunk with non-empty `choices` — translator_cc.go:941–945 |
| `item.started` (message) | ✅ emitted | On first text delta — translator_cc.go:1096–1101 |
| `item.started` (tool call) | ✅ emitted | On first tool call fragment — translator_cc.go:1149–1155 |
| `item.started` (reasoning) | ✅ emitted | On first `reasoning_content` fragment — translator_cc.go:1058–1064 |
| `item.delta` text | ✅ emitted | translator_cc.go:1105–1111 |
| `item.delta` arguments | ✅ emitted | translator_cc.go:1160–1166 |
| `item.delta` reasoning | ✅ emitted | translator_cc.go:1068–1074 |
| `item.completed` (message) | ✅ emitted | `closeMsgItem` on `[DONE]` — translator_cc.go:1172–1187 |
| `item.completed` (tool call) | ✅ emitted | `closeToolItem` on `[DONE]` — translator_cc.go:1206–1220 |
| `item.completed` (reasoning) | ✅ emitted | `closeReasoningItem` on `[DONE]` — translator_cc.go:1189–1204 |
| `generation.completed` | ✅ emitted | `handleDone` — translator_cc.go:1036–1042 |
| `error` event | ⚠️ not emitted | No mapping from CC error chunks to `v1.EventError`; parse errors return a Go error but no canonical `error` event — translator_cc.go:905–1001 |
| `generation.created` emitted on chunk with empty `choices` | ⚠️ missed | Guard at translator_cc.go:949 returns early before `lifecycleEmitted` check; if first chunk ever has empty choices, `generation.created` is never emitted |
| `stream_options.include_usage` | ⚠️ not injected | `ccToCanonicalStream` does not inject `stream_options: {include_usage: true}` into the upstream request, so `s.lastUsage` will usually be nil — translator_cc.go:892–903 |
| CC chunks beyond `choices[0]` | ⚠️ dropped | Only `ccChunk.Choices[0]` processed — translator_cc.go:953 |
| `system_fingerprint` / `service_tier` in chunks | ⚠️ dropped | Not surfaced — translator_cc.go:916 |
| Ordering: reasoning before message before tool calls | ✅ correct | Enforced by `handleTextDelta` closing reasoning on first text, and `handleToolCallDelta` closing both — translator_cc.go:1082–1087, 1119–1127 |
| `generation.created` emitted before first `item.started` | ✅ correct | translator_cc.go:941–945 placed before delta processing |

### From canonical (NewFromCanonicalStream)

```
func (CCTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
    return nil // identity: canonical → CC is not a production path
}
```

— translator_cc.go:436–438. Returns `nil`. **Any caller that invokes the returned function will panic with a nil pointer dereference.** This is "not a production path" per the comment, but there is no guard asserting this is never called. If cross-shape routing (e.g. canonical client → CC upstream) is ever attempted, this will panic at runtime.

---

## ⚠️ Silently dropped (no error, no log)

- **`ChatResponseMessage.Annotations` (URL citations)** — URL citations from OpenAI web search responses are parsed by `types.go:162` but `ccChoiceToCanonicalOutput` never reads `msg.Annotations`. Lost on every web-search response. `translator_cc.go:742–784`
- **`input_audio` content parts** — `ccContentToParts` has no `case "input_audio"` branch; audio parts in user messages are silently omitted from canonical input. `translator_cc.go:488–513`
- **`messages[].name`** — CC's per-message `name` field is present in `ChatMessage` (types.go:51) but never copied to any canonical field. `translator_cc.go:520`
- **`usage.prompt_tokens_details.audio_tokens`** — `ccUsageToCanonical` reads `PromptDetails.CachedTokens` but not `AudioTokens`; audio input tokens are silently zeroed in `ParseResponse` and stream translation. Compare: `ExtractTokens` in tokens.go:64 correctly maps this. `translator_cc.go:797–811`
- **`usage.completion_tokens_details.audio_tokens`**, **`accepted_prediction_tokens`**, **`rejected_prediction_tokens`** — Same omission in `ccUsageToCanonical`. `translator_cc.go:812–818`
- **`v1.Response.Error` in SerializeResponse** — If a canonical `Response` carries an `Error` (e.g. from a failed upstream), `SerializeResponse` produces a valid-looking `ChatResponse` with no choices and no error envelope. `translator_cc.go:341–423`
- **`v1.Response.IncompleteDetails` in SerializeResponse** — `incomplete_details` is not surfaced in the CC response wire shape. `translator_cc.go:341–423`
- **`OutputTextPart.Annotations` in SerializeResponse** — Annotations on output text parts are never written to `ChatResponseMessage.Annotations`. `translator_cc.go:378–385`
- **`ReasoningConfig.Summary` and `.BudgetTokens`** — Both fields are part of canonical `ReasoningConfig` but `SerializeRequest` only emits `Effort`. `translator_cc.go:248–250`
- **`OutputConfig.Verbosity`** — Parsed from canonical but never emitted. `translator_cc.go:251–257`
- **`n`, `logit_bias`, `logprobs`, `top_logprobs`, `service_tier`, `store`, `stream_options`** — All present in `FullChatRequest` struct but never read in `ParseRequest`. `translator_cc.go:37–214`
- **`system_fingerprint` and `service_tier` in ParseResponse** — Never propagated to canonical Extensions. `translator_cc.go:305–336`
- **Reasoning items in input (SerializeRequest)** — `case *v1.Reasoning:` in `canonicalItemsToCC` is a comment-only no-op drop. `translator_cc.go:602–604`
- **`stream_options.include_usage` not injected upstream** — `ccToCanonicalStream` never adds `stream_options: {include_usage: true}` to the outgoing request, so usage data will be absent from all streaming responses unless the caller independently sets it. `translator_cc.go:892–903`

---

## 🔒 Hardcoded / defaulted

- **`resp.Object = "response"`** in `ParseResponse` — vendor value (`"chat.completion"`) replaced unconditionally. `translator_cc.go:313`
- **`cc.Object = "chat.completion"`** in `SerializeResponse`. `translator_cc.go:343`
- **`"msg_" + ccID`** as canonical Message ID in `ParseResponse`. `translator_cc.go:759`
- **`tool.Function.Parameters = json.RawMessage("{}")`** when nil in `ParseRequest` — translator_cc.go:110
- **`finishReason = "stop"`** as default in `SerializeResponse` for unrecognised canonical finish reasons. `translator_cc.go:368–370`
- **`time.Now().Unix()`** fallback when `cc.Created == 0` in both `ParseResponse` (translator_cc.go:317) and `ccToCanonicalStream` (translator_cc.go:931)
- **`resp_<unix>`** synthetic response ID when `ccChunk.ID == ""`. `translator_cc.go:934–936`
- **`StatusCompleted / FinishReasonStop`** emitted for empty-choices response. `translator_cc.go:327–329`
- **`role = "assistant"`** hardcoded on all serialized CC `ChatResponseMessage`. `translator_cc.go:353`

---

## ⛔ Unsupported (explicit error)

- **`ServerTool` in SerializeRequest** — `fmt.Errorf("cc serialize_request: unsupported tool type %T", tool)`. `translator_cc.go:263`
- **`MCPTool` in SerializeRequest** — same path as above. `translator_cc.go:263`
- **Invalid JSON in stop field** — silently ignored (both parse attempts fail → `sampling.Stop` stays nil). `translator_cc.go:90–99`
- **model missing in ParseRequest** — returns explicit error. `translator_cc.go:43`
- **model missing in SerializeRequest** — returns explicit error. `translator_cc.go:219`

---

## Round-trip fidelity

**canonical → OpenAI → canonical (serialize then parse back):**
- `Message.CacheConfig` lost (by design; OpenAI ignores it)
- `Message.ProviderData` lost
- `FunctionCall.ProviderData` lost
- `ReasoningConfig.Summary` and `.BudgetTokens` lost
- `OutputConfig.Verbosity` lost
- `TopK` lost (no CC equivalent)
- Developer-role messages become system messages and come back as `instructions` (first) or developer messages (subsequent) — ordering semantics may shift
- `Extensions` on request lost
- `Reasoning` input items dropped

**OpenAI → canonical → OpenAI (parse then serialize back):**
- `Annotations` on response messages lost (URL citations)
- `system_fingerprint` lost
- `service_tier` lost
- `logprobs` / `top_logprobs` lost
- `n`, `logit_bias`, `store`, `stream_options`, `service_tier` lost
- Audio usage tokens (`audio_input`, `audio_output`) lost
- `accepted_prediction_tokens`, `rejected_prediction_tokens` lost
- `messages[].name` lost
- `input_audio` content parts lost
- `incomplete_details` lost (no CC field)
- Error body lost (no `v1.Response.Error` → CC error envelope mapping)

---

## Recommendations (prioritized)

**P1 — Data loss that will surprise callers today:**

1. **Map `ChatResponseMessage.Annotations` → `OutputTextPart.Annotations`** in `ccChoiceToCanonicalOutput`. URL citations are a first-class OpenAI web-search output; silent drop is unacceptable. `translator_cc.go:742–784`

2. **`ccUsageToCanonical` is inconsistent with `ExtractTokens`** — `ccUsageToCanonical` (used by `ParseResponse` and stream) does not map `audio_input`, `audio_output`, `accepted_prediction`, `rejected_prediction`. These four usage keys exist in `tokens.go` but are dead in the translator path. Either consolidate both onto one shared helper or fix `ccUsageToCanonical`. `translator_cc.go:794–819` vs `tokens.go:28–82`

3. **Inject `stream_options: {include_usage: true}` in `SerializeRequest` when `OutputMode == stream`**, or document that usage will always be nil in streaming canonical responses. Currently `s.lastUsage` is always nil in streaming unless the upstream client set it independently. `translator_cc.go:289–292`

4. **`NewFromCanonicalStream()` returning `nil` is a latent panic.** Add a guard or return a no-op identity function with a `// not a production path` comment that returns an explicit error on call. `translator_cc.go:436–438`

**P2 — Silent drops that should be logged or explicitly documented:**

5. **`input_audio` content parts** — add an explicit `case "input_audio"` that returns an error or logs a warning; currently the part is silently discarded. `translator_cc.go:488–513`

6. **Serialize `v1.Response.Error` in `SerializeResponse`** — a canonical error response currently produces an empty-choices CC response with no error field; callers will misinterpret it as a successful empty completion.

7. **Propagate `system_fingerprint` and `service_tier`** via `Extensions` in `ParseResponse` so they are available to observers without loss.

8. **`messages[].name`** — either map to a canonical extension key or log a warning on drop; it carries meaningful disambiguation in multi-agent conversations.

**P3 — Minor / by-design gaps worth documenting:**

9. **`ReasoningConfig.Summary` and `.BudgetTokens`** — CC has no direct equivalent today; add a code comment marking these as intentional no-ops rather than silent drops.

10. **`OutputConfig.Verbosity`** — same; comment-mark as CC-unsupported.

11. **`n > 1` responses** — `choices[1..n]` are silently dropped; add a comment or return an error for `n > 1` requests at parse time.

12. **`logprobs` / `top_logprobs`** — if these are ever needed downstream (e.g. for scoring), they need a canonical home (likely `Extensions`). Currently parsed and discarded.
