# Anthropic Messages — Canonical Fidelity Audit

> Audited 2026-05-25 against `pkg/adapters/anthropic/translator_canonical.go`.
> Critical, honest map of what survives canonical translation.
> Every claim cites file:line.

---

## Verdict

The adapter is **broadly correct** for the common path (text, tool-use, streaming,
prompt caching, thinking signatures). Six material gaps were found; two are silent
data loss (no error, no log) that can produce wrong model behaviour or surprise
callers:

1. **`ToolsConfig.Parallel` is never wired into `canonicalToolChoiceToAnthropic`** — `disable_parallel_tool_use` is only set when the caller passes an explicit `*bool` argument, but `SerializeRequest` always passes `nil`. Callers cannot disable parallel tool use.
2. **`stop_sequence` value is silently lost** on the Anthropic→canonical→Anthropic round-trip: `stop_sequence` string from `message_delta` is captured in `handleMessageDelta` but never stored and never emitted in `handleGenerationCompleted`.
3. **`redacted_thinking` blocks are silently dropped** in both `ParseResponse` (line 467) and the stream `handleContentBlockStart` (line 746) with no error or log.
4. **`Output.Format` (structured output / JSON mode) is entirely unimplemented** — the `ModelOpts.Output` field is never read in `SerializeRequest`.
5. **`FunctionTool.Strict`** is never forwarded to the Anthropic wire.
6. **`Seed`, `FrequencyPenalty`, `PresencePenalty`** are accepted by canonical but silently discarded — Anthropic does not support them, but there is no error or warning.

---

## Request: canonical → Anthropic (`SerializeRequest`)

| Canonical element | Status | Notes + file:line |
|---|---|---|
| `Model` (first ref) | ✅ | `translator_canonical.go:292` |
| `Instructions` (system) | ✅ | Plain string; coerced to block array when `CacheConfig.Instructions=true` — `translator_canonical.go:378–384` |
| `OutputMode` → `stream` | ✅ | `translator_canonical.go:298–301` |
| `User` → `metadata.user_id` | ✅ | `translator_canonical.go:303–305` |
| `Metadata` (map[string]string) | ⛔ | Not read; only `User` is forwarded. Relay-level metadata does not reach the wire. |
| `Extensions` | ⚠️ | Never read in `SerializeRequest`. Dropped silently. `translator_canonical.go:287–387` |
| `SamplingParams.Temperature` | ✅ | `translator_canonical.go:312` |
| `SamplingParams.TopP` | ✅ | `translator_canonical.go:313` |
| `SamplingParams.TopK` | ✅ | `translator_canonical.go:314` |
| `SamplingParams.MaxTokens` | ✅ / 🔒 | Forwarded; defaults to `4096` when nil — see Hardcoded section |
| `SamplingParams.Stop` → `stop_sequences` | ✅ | `translator_canonical.go:319` |
| `SamplingParams.Seed` | ⚠️ | Anthropic has no `seed` param; silently dropped (no error). `model_opts.go:29` exists but `translator_canonical.go:309–320` never reads it. |
| `SamplingParams.FrequencyPenalty` | ⚠️ | Silently dropped — Anthropic unsupported. |
| `SamplingParams.PresencePenalty` | ⚠️ | Silently dropped — Anthropic unsupported. |
| `ReasoningConfig.BudgetTokens` | ✅ | `translator_canonical.go:325` |
| `ReasoningConfig.Effort` | ⚠️ | Stored in `thinking.Type` but never translated to `budget_tokens`; comment at line 329 says "keep as Effort passthrough" but Anthropic wire has no `effort` field — the value is effectively dropped. `translator_canonical.go:327–332` |
| `ReasoningConfig.Summary` | ⚠️ | Not read. Anthropic has no equivalent; silently dropped. |
| `ToolsConfig.Definitions` → `tools[]` | ✅ | `translator_canonical.go:334–352` |
| `FunctionTool.Strict` | ⚠️ | Never forwarded to Anthropic tool block. `translator_canonical.go:341–350` |
| `ToolsConfig.Choice` → `tool_choice` | ✅ | `translator_canonical.go:349–352` |
| `ToolsConfig.Parallel` → `disable_parallel_tool_use` | ⛔ | `canonicalToolChoiceToAnthropic` accepts a `*bool` arg but `SerializeRequest` always passes `nil` at line `350`: `canonicalToolChoiceToAnthropic(tc.Choice, nil)`. The param is dead. |
| `OutputConfig.Format` (JSON mode / json_schema) | ⛔ | `ModelOpts.Output` is never read in `SerializeRequest`. Structured output entirely unimplemented. |
| `CacheConfig.Instructions` | ✅ | `translator_canonical.go:379–383` |
| `CacheConfig.Tools` | ✅ | Breakpoint on last tool — `translator_canonical.go:357–359` |
| `ItemCacheConfig.Anchor` (per-Message) | ✅ | `translator_canonical.go:1624–1636` |
| `Message.ProviderData` | ⚠️ | Not forwarded to Anthropic wire (no equivalent field on message blocks). Silent drop. |
| `Message` developer/system role | ✅ | Appended to system text — `translator_canonical.go:1604–1618` |
| `FunctionCall` item → `tool_use` | ✅ | Batched into assistant message — `translator_canonical.go:1545–1568` |
| `FunctionCallOutput` item → `tool_result` | ✅ | `translator_canonical.go:1571–1595` |
| `FunctionCallOutput.Content` ([]Part) | ✅ | Text parts concatenated — `translator_canonical.go:1578–1582` |
| `FunctionCallOutput.Content` image parts | ⚠️ | Silently dropped — only `*v1.TextPart` is extracted in `flushToolResults`. Image content in tool results is lost. `translator_canonical.go:1584` |
| `Reasoning` item (input) | ⚠️ | Explicitly dropped with comment "Anthropic manages its own thinking" — `translator_canonical.go:1647–1649`. Cross-vendor Reasoning input is silently discarded; no error. |
| `FilePart` (base64 PDF) | ✅ | → `document` block with `base64` source — `translator_canonical.go:1709–1722` |
| `FilePart` (URL) | ✅ | → `document` block with `url` source — `translator_canonical.go:1723–1730` |
| `FilePart.FileID` | ⛔ | No Anthropic equivalent; returns error only if neither `FileData` nor `FileURL` is set. If `FileID` alone is set: error. `translator_canonical.go:1732` |
| `FilePart.Filename` | ⚠️ | Not forwarded to Anthropic `document` block. |
| `ImagePart` (data-URI) | ✅ | Decoded to `base64` image block — `translator_canonical.go:1739–1757` |
| `ImagePart` (plain URL) | ✅ | Forwarded as `url` image block — `translator_canonical.go:1757–1763` |
| `ImagePart.Detail` | ⚠️ | Anthropic has no `detail` field; silently dropped. |

---

## Request: Anthropic → canonical (`ParseRequest`)

| Anthropic wire field | Status | Notes + file:line |
|---|---|---|
| `model` | ✅ | `translator_canonical.go:172–173` |
| `stream` | ✅ | `translator_canonical.go:176–181` |
| `system` (string or blocks) | ✅ | Extracted as plain string via `anthropicExtractSystemText` — `translator_canonical.go:183–185` |
| System blocks with `cache_control` | ⚠️ | `anthropicExtractSystemText` extracts text only; `cache_control` markers dropped. Per design: callers express cache intent via `cache_config`, not vendor fields. `translator_canonical.go:1179–1197` |
| `metadata.user_id` | ✅ | `translator_canonical.go:188–195` |
| `metadata.*` (other fields) | ⚠️ | Only `user_id` extracted. Any other metadata keys dropped. |
| `max_tokens` | ✅ | `translator_canonical.go:216–219` |
| `temperature`, `top_p`, `top_k`, `stop_sequences` | ✅ | `translator_canonical.go:204–226` |
| `tools[]` | ✅ | `translator_canonical.go:230–246` |
| Server tools (type != "function") | ✅ | Mapped to `*v1.ServerTool` — `translator_canonical.go:1213–1215` |
| `tool_choice` | ✅ | `translator_canonical.go:241–244` |
| `tool_choice.disable_parallel_tool_use` | ⚠️ | Not parsed. `anthropicParseToolChoice` reads only `type`/`name` — `translator_canonical.go:1228–1248`. `ToolsConfig.Parallel` is never set. |
| `thinking.type=enabled` | ✅ | `translator_canonical.go:249–267` |
| `thinking.budget_tokens` | ✅ | `translator_canonical.go:258–261` |
| `thinking.effort` | ⚠️ | Passed through as `ReasoningConfig.Effort`; Anthropic does not actually use this field. |
| Messages → canonical `Input` | ✅ | `translator_canonical.go:274–280` |
| User message text | ✅ | |
| User message image (base64 & URL) | ✅ | `translator_canonical.go:1511–1535` |
| User message `tool_result` blocks | ✅ | `translator_canonical.go:1376–1433` |
| `tool_result` image content | ⚠️ | `splitToolResults` calls `anthropicContentToCanonicalParts` and then only extracts `*v1.TextPart.Text` — line 1405. Image parts in tool results are dropped when building `FunctionCallOutput.Output`. |
| Assistant message text | ✅ | |
| Assistant message `tool_use` blocks | ✅ | `translator_canonical.go:1476–1484` |
| Assistant message `thinking` blocks | ✅ | Signature preserved in `ProviderData` — `translator_canonical.go:1486–1500` |
| `redacted_thinking` in messages | ⚠️ | Silently dropped — `anthropicContentToCanonicalParts` has no case for it. |
| Unknown message roles | ⚠️ | Silently mapped to `RoleUser` — `translator_canonical.go:1318–1323` |
| `betas`, `system` non-text blocks | ⚠️ | Dropped — only `type=text` system blocks are extracted. |

---

## Response: Anthropic → canonical (`ParseResponse`)

| Anthropic response field | Status | Notes + file:line |
|---|---|---|
| `id` | ✅ | `translator_canonical.go:399` |
| `model` | ✅ | `translator_canonical.go:402` |
| `type` | ⚠️ | Not forwarded. `Response.Object` hardcoded to `"response"` — `translator_canonical.go:400` |
| `role` | ⚠️ | Not captured (always "assistant"; implied by item type). |
| `created_at` | 🔒 | Hardcoded to `time.Now().Unix()` — not derived from wire. `translator_canonical.go:401`. Anthropic does not return a timestamp. |
| `stop_reason` → `Status`/`FinishReason`/`IncompleteDetails` | ✅ | See stop_reason mapping table below. |
| `stop_sequence` value | ⚠️ | Parsed into `anthropicFullResp.StopSeq` (`translator_canonical.go:112`) but never forwarded to canonical. The actual stop sequence string is silently dropped. |
| `content[].text` block | ✅ | → `*v1.Message` with `OutputTextPart` — `translator_canonical.go:412–426` |
| `content[].text.citations` | ✅ | url_citation mapped — `translator_canonical.go:415` |
| `content[].text.citations` (char_location, page_location) | ⚠️ | Silently dropped — comment at `translator_canonical.go:1829` |
| `content[].tool_use` block | ✅ | → `*v1.FunctionCall` — `translator_canonical.go:427–439` |
| `content[].thinking` block + signature | ✅ | → `*v1.Reasoning` with `ProviderData` — `translator_canonical.go:441–464` |
| `content[].redacted_thinking` | ⚠️ | Silently dropped with comment "Cannot faithfully represent" — `translator_canonical.go:466–467` |
| `content[].server_tool_use` | ⚠️ | Silently dropped — `translator_canonical.go:469–470` |
| Unknown block types | ⚠️ | Silently dropped — `translator_canonical.go:472–473` |
| `usage.input_tokens` | ✅ | → `usage["input"]` — `translator_canonical.go:496` |
| `usage.output_tokens` | ✅ | → `usage["output"]` — `translator_canonical.go:499` |
| `usage.cache_read_input_tokens` | ✅ | → `usage["cache_read"]` — `translator_canonical.go:502` |
| `usage.cache_creation_input_tokens` | ✅ | → `usage["cache_creation"]` — `translator_canonical.go:505` |

### `stop_reason` mapping

| Anthropic `stop_reason` | `Status` | `FinishReason` | `IncompleteDetails.Reason` |
|---|---|---|---|
| `end_turn` | `completed` | `stop` | — |
| `stop_sequence` | `completed` | `stop` | — |
| `""` (empty) | `completed` | `stop` | — |
| `max_tokens` | `incomplete` | `length` | `max_output_tokens` |
| `tool_use` | `completed` | `tool_calls` | — |
| `refusal` | `completed` | `refusal` | — |
| `pause_turn` | `incomplete` | `""` | `pause_turn` |
| anything else | `completed` | `stop` | — |

Note: `stop_sequence` and `end_turn` collapse to the same canonical `stop` finish reason — there is no way for a downstream caller to distinguish them. The actual matched sequence string is also lost (see above).

---

## Response: canonical → Anthropic (`SerializeResponse`)

| Canonical field | Status | Notes + file:line |
|---|---|---|
| `ID` | ✅ | `translator_canonical.go:519` |
| `Model` | ✅ | `translator_canonical.go:522` |
| `CreatedAt` | ⚠️ | Not emitted. Anthropic response shape has no `created_at` field. |
| `Status` / `FinishReason` → `stop_reason` | ✅ | `translator_canonical.go:526` |
| `Output[].Message` text parts | ✅ | `translator_canonical.go:534–542` |
| `Output[].FunctionCall` → `tool_use` | ✅ | `translator_canonical.go:551–566` |
| `Output[].Reasoning` → `thinking` (with ProviderData) | ✅ | `translator_canonical.go:567–595` |
| `Output[].Reasoning` (no ProviderData, has Content) | ✅ | Fallback to `Content` — `translator_canonical.go:587–590` |
| `Output[].Reasoning` (no ProviderData, no Content) | ✅ | Fallback to `Summary[0].Text` — `translator_canonical.go:591–594` |
| `Usage` → `input_tokens`/`output_tokens` | ✅ | `translator_canonical.go:605–617` |
| `Usage.cache_read` / `cache_creation` | ✅ | `translator_canonical.go:610–615` |
| `Error` | ⚠️ | Not emitted. Canonical `Response.Error` has no home in `SerializeResponse`. |
| `IncompleteDetails` | ⚠️ | Indirectly represented via `stop_reason`; the `Reason` string is not exposed on the wire body. |
| `Extensions` | ⚠️ | Not emitted. |
| `Object` | 🔒 | Hardcoded to `"message"` — `translator_canonical.go:520` |
| `role` | 🔒 | Hardcoded to `"assistant"` — `translator_canonical.go:521` |

---

## Streaming

### To canonical (`NewToCanonicalStream`)

| Anthropic event | Canonical event(s) | Notes + file:line |
|---|---|---|
| `ping` | dropped | `translator_canonical.go:689` |
| `message_start` | `generation.created` | ID, model, input/cache token counts captured — `translator_canonical.go:696–728` |
| `content_block_start` (text) | `item.started` (ItemTypeMessage) | `translator_canonical.go:769–772` |
| `content_block_start` (tool_use) | `item.started` (ItemTypeFunctionCall) | Name/CallID captured — `translator_canonical.go:759–763`, `769–771` |
| `content_block_start` (thinking) | `item.started` (ItemTypeReasoning) | `translator_canonical.go:773–774` |
| `content_block_start` (server_tool_use) | dropped | `translator_canonical.go:746` |
| `content_block_start` (redacted_thinking) | dropped | `translator_canonical.go:746` |
| `content_block_delta` (text_delta) | `item.delta` (DeltaKindText) | `translator_canonical.go:810–814` |
| `content_block_delta` (input_json_delta) | `item.delta` (DeltaKindArguments) | `translator_canonical.go:815–819` |
| `content_block_delta` (thinking_delta) | `item.delta` (DeltaKindReasoning) | `translator_canonical.go:820–824` |
| `content_block_delta` (signature_delta) | dropped | No case in delta switch — `translator_canonical.go:809–825`. The thinking signature is only known at block-stop time but is not stored during streaming. |
| `content_block_stop` | `item.completed` | `translator_canonical.go:835–876` |
| `content_block_stop` (thinking) | `item.completed` (Reasoning) | **Signature is NOT captured** — `Reasoning.ProviderData` is nil in stream path. `translator_canonical.go:859–865` |
| `message_delta` | (internal) | `stop_reason` and `output_tokens` stored for later — `translator_canonical.go:878–893` |
| `message_stop` | `generation.completed` | `translator_canonical.go:895–932` |
| `pause_turn` stop_reason | `generation.completed` (StatusIncomplete) | `IncompleteDetails` is parsed but discarded with `_ = incomplete` — `translator_canonical.go:922–929` |
| `error` | `error` | `translator_canonical.go:934–951` |
| unknown events | dropped | `translator_canonical.go:691` |

**Critical gap**: `stop_sequence` value from `message_delta.delta.stop_sequence` is never captured in `handleMessageDelta`. The struct only reads `StopReason` and `OutputTokens` — `translator_canonical.go:879–893`.

**Critical gap**: Thinking signature is not captured during stream path. The `content_block_delta` event emits a `signature_delta` type (Anthropic API) that has no case in the delta switch, so the signature is silently dropped. The `item.completed` event for Reasoning items carries no `ProviderData` — `translator_canonical.go:859–865`. Same-vendor round-trips that depend on the signature (extended thinking continuation) **will fail silently** in streaming mode.

### From canonical (`NewFromCanonicalStream`)

| Canonical event | Anthropic event(s) | Notes + file:line |
|---|---|---|
| `generation.created` | `message_start` + `ping` | Input token counts hardcoded to `0` — `translator_canonical.go:1004–1016` |
| `item.started` (ItemTypeMessage) | `content_block_start` (text) | `translator_canonical.go:1034–1035` |
| `item.started` (ItemTypeFunctionCall) | `content_block_start` (tool_use) | Requires `Name` in `ItemStartedEvent` — `translator_canonical.go:1036–1041` |
| `item.started` (ItemTypeReasoning) | `content_block_start` (thinking) | `translator_canonical.go:1043–1044` |
| `item.delta` (DeltaKindText) | `content_block_delta` (text_delta) | `translator_canonical.go:1066–1068` |
| `item.delta` (DeltaKindArguments) | `content_block_delta` (input_json_delta) | `translator_canonical.go:1069–1071` |
| `item.delta` (DeltaKindReasoning) | `content_block_delta` (thinking_delta) | `translator_canonical.go:1072–1074` |
| `item.completed` | `content_block_stop` | Only `Index` read — Item content ignored. `translator_canonical.go:1091–1104` |
| `generation.completed` | `message_delta` + `message_stop` | `translator_canonical.go:1107–1136` |
| `generation.completed` (pause_turn) | `message_delta` with `stop_reason: "pause_turn"` | `translator_canonical.go:1113–1115` |
| `generation.completed` usage | `message_delta.usage` | Only `output_tokens` forwarded — `translator_canonical.go:1118–1122`; `input_tokens`, `cache_read`, `cache_creation` **not** emitted in `message_delta.usage` |
| `error` | `error` | `translator_canonical.go:1139–1151` |

---

## Prompt caching fidelity

The adapter correctly implements Anthropic's three-tier breakpoint model:

| Cache intent | Breakpoint placement | Wire field | Budget cost | File:line |
|---|---|---|---|---|
| `CacheConfig.Instructions=true` | On system block (coerced to array if string) | `system[last].cache_control` | 1 | `translator_canonical.go:379–383` |
| `CacheConfig.Tools=true` | On last tool definition | `tools[last].cache_control` | 1 | `translator_canonical.go:357–359` |
| `Message.CacheConfig.Anchor=true` | On last content block of that message | Message content block `.cache_control` | 1 per anchor | `translator_canonical.go:1624–1636` |

**Budget arithmetic**: Anthropic allows 4 simultaneous breakpoints. The adapter places one for instructions + one for tools + one per anchored message. There is no guard preventing callers from anchoring more than 2 messages and exceeding the 4-breakpoint budget. Anthropic silently ignores excess breakpoints (oldest evicted), but the adapter does not warn.

**Inbound cache markers ignored by design**: `ParseRequest` never reverse-maps `cache_control` markers back to canonical `cache_config`. This is documented at `translator_canonical.go:9–11`. A client sending a native Anthropic request with `cache_control` breakpoints will have them stripped on `ParseRequest` and not re-emitted on the next `SerializeRequest`. This is intentional but worth confirming with callers.

**Usage keys**: `cache_read` and `cache_creation` propagate correctly in both sync (`ParseResponse`) and stream (`handleMessageStart` + `handleMessageStop`) paths.

---

## Thinking/reasoning fidelity

### Non-streaming path

| Step | Behaviour | File:line |
|---|---|---|
| Parse Anthropic response `thinking` block | `Content` + `Summary` populated; `ProviderData` = `{"type":"thinking","thinking":"...","signature":"..."}` | `translator_canonical.go:441–464` |
| Serialize canonical `Reasoning` back to Anthropic | If `ProviderData` present and `type=thinking`, restores full `thinking`+`signature` block | `translator_canonical.go:568–585` |
| Cross-vendor (no ProviderData) | Falls back to `Content` text without signature | `translator_canonical.go:587–590` |
| `redacted_thinking` block | **Silently dropped** — `translator_canonical.go:466–467` |

Non-streaming same-vendor round-trip is **correct** as long as `ProviderData` is preserved by the calling code.

### Streaming path

| Step | Behaviour | File:line |
|---|---|---|
| `content_block_start` (thinking) | `item.started` emitted correctly | `translator_canonical.go:773–774` |
| `content_block_delta` (thinking_delta) | `item.delta` (DeltaKindReasoning) emitted; text accumulated | `translator_canonical.go:820–824` |
| `content_block_delta` (signature_delta) | **Silently dropped** — no case in delta type switch | `translator_canonical.go:809–825` |
| `content_block_stop` (thinking) | `item.completed` emitted; `Reasoning.ProviderData` is **nil** | `translator_canonical.go:859–865` |

**The streaming path does not capture the thinking signature.** Any downstream consumer (e.g., multi-turn extended thinking) that reads `Reasoning.ProviderData` from a streamed response will find it nil and be unable to continue thinking across turns. This is a correctness bug for the extended-thinking continuation use case.

---

## ⚠️ Silently dropped (no error, no log)

| Element | Where | File:line |
|---|---|---|
| `redacted_thinking` blocks (response) | `ParseResponse` | `translator_canonical.go:466–467` |
| `redacted_thinking` blocks (stream) | `handleContentBlockStart` | `translator_canonical.go:746` |
| `server_tool_use` blocks (response) | `ParseResponse` | `translator_canonical.go:469–470` |
| `server_tool_use` blocks (stream) | `handleContentBlockStart` | `translator_canonical.go:746` |
| `stop_sequence` matched string (response) | `ParseResponse` | Field parsed at `translator_canonical.go:112` but never forwarded |
| `stop_sequence` matched string (stream) | `handleMessageDelta` | `translator_canonical.go:879–893` — struct never reads `stop_sequence` |
| Thinking `signature_delta` (stream) | `handleContentBlockDelta` | `translator_canonical.go:809–825` — no case for `signature_delta` |
| `ToolsConfig.Parallel` → `disable_parallel_tool_use` | `SerializeRequest` | `translator_canonical.go:350` — always passes `nil` |
| `tool_choice.disable_parallel_tool_use` (inbound) | `anthropicParseToolChoice` | `translator_canonical.go:1228–1248` |
| `SamplingParams.Seed` | `SerializeRequest` | Never read |
| `SamplingParams.FrequencyPenalty` | `SerializeRequest` | Never read |
| `SamplingParams.PresencePenalty` | `SerializeRequest` | Never read |
| `ReasoningConfig.Effort` | `SerializeRequest` | Set on struct but Anthropic wire has no `effort` field — `translator_canonical.go:327–332` |
| `ReasoningConfig.Summary` | `SerializeRequest` | Never read |
| `OutputConfig.Format` (structured output) | `SerializeRequest` | `ModelOpts.Output` never read — `translator_canonical.go:287–387` |
| `FunctionTool.Strict` | `SerializeRequest` | `translator_canonical.go:341–350` |
| `FilePart.Filename` | `canonicalPartToAnthropicBlock` | `translator_canonical.go:1709–1731` |
| `ImagePart.Detail` | `canonicalImageURLToAnthropicBlock` | Anthropic has no detail field |
| `Message.ProviderData` | `canonicalItemsToAnthropic` | Not forwarded to wire |
| Inbound `cache_control` markers | `ParseRequest` / `anthropicExtractSystemText` | By design; see Prompt caching section |
| Image parts in `tool_result` content | `splitToolResults` | `translator_canonical.go:1403–1407` |
| Image parts in `FunctionCallOutput.Content` (serialize) | `flushToolResults` | `translator_canonical.go:1578–1583` |
| `char_location` / `page_location` citations | `anthropicCitationsToCanonical` | `translator_canonical.go:1829` |
| Citation annotations on `content_block_stop` in stream | `handleContentBlockStop` | `OutputTextPart` assembled with no annotations — `translator_canonical.go:848–850` |
| `IncompleteDetails` in streaming `generation.completed` | `handleMessageStop` | `_ = incomplete` at `translator_canonical.go:928` |
| `usage.input_tokens` in `from-canonical message_start` | `handleGenerationCreated` | Hardcoded to `0` — `translator_canonical.go:1006` |
| `usage.cache_read/cache_creation` in `from-canonical message_delta` | `handleGenerationCompleted` | Only `output_tokens` forwarded — `translator_canonical.go:1118–1122` |
| `Reasoning` items in canonical input | `canonicalItemsToAnthropic` | `translator_canonical.go:1647–1649` |
| Unknown message roles | `anthropicMessagesToCanonical` | Mapped to `RoleUser` silently — `translator_canonical.go:1318–1323` |
| `Request.Metadata` map | `SerializeRequest` | Only `User` forwarded |
| `Request.Extensions` | `SerializeRequest` | Never read |
| `Response.Error` | `SerializeResponse` | Not emitted |
| `Response.Extensions` | `SerializeResponse` | Not emitted |

---

## 🔒 Hardcoded / defaulted

| Element | Value | Risk | File:line |
|---|---|---|---|
| `max_tokens` default | model max (was `4096`) | **FIXED.** Dispatch now seeds canonical `SamplingParams.MaxTokens` from the catalog model's `MaxOutputTokens` when the caller leaves it unset (`app/httpapi/inference` `applyOutputDefaults`). The `4096` constant remains only as a last-resort fallback for models with no published max. | `app/httpapi/inference/dispatch.go`, `translator_canonical.go:32` |
| `Response.Object` | `"response"` | Low — Anthropic shape uses `"message"` on the wire but this field is only visible in canonical response, not re-serialized to Anthropic clients. | `translator_canonical.go:400` |
| `Response.CreatedAt` | `time.Now().Unix()` | Low — Anthropic provides no timestamp. Every relay re-read of the same response gets a different `created_at`. | `translator_canonical.go:401` |
| `SerializeResponse` `type` | `"message"` | Low — correct for Anthropic wire. | `translator_canonical.go:520` |
| `SerializeResponse` `role` | `"assistant"` | Low — correct for Anthropic Messages. | `translator_canonical.go:521` |
| `message_start` input token counts (from-canonical stream) | `0` | Low for passthrough; **medium if used for billing display** — a client receiving a proxied canonical-to-Anthropic stream sees `input_tokens: 0` in the opening `message_start` frame. Tokens do arrive correctly in `message_delta`. | `translator_canonical.go:1006` |
| `FilePart` media_type default | `"application/pdf"` | Low — `FilePart.MediaType` overrides it; default is a reasonable assumption for documents. | `translator_canonical.go:1711–1714` |

---

## ⛔ Unsupported (explicit error)

| Condition | Error | File:line |
|---|---|---|
| `model` missing in `ParseRequest` | `"anthropic parse_request: model is required"` | `translator_canonical.go:168–170` |
| `model` missing in `SerializeRequest` | `"anthropic serialize_request: model is required"` | `translator_canonical.go:288–290` |
| Unsupported tool type (not `*v1.FunctionTool`) in `SerializeRequest` | `"anthropic serialize_request: unsupported tool type %T"` | `translator_canonical.go:337–338` |
| `FilePart` with no `FileData` and no `FileURL` | `"anthropic serialize_request: file part has no data or URL"` | `translator_canonical.go:1732` |
| Unsupported part type | `"anthropic serialize_request: unsupported part type %T"` | `translator_canonical.go:1734` |

Note: `ServerTool` and `MCPTool` in `ToolsConfig.Definitions` will hit the "unsupported tool type" error on `SerializeRequest` because the switch only handles `*v1.FunctionTool` (`translator_canonical.go:335`).

---

## Round-trip fidelity

### canonical → Anthropic → canonical

| Field | Round-trips? | Notes |
|---|---|---|
| Text content | ✅ | |
| Tool definitions + tool_choice | ✅ | |
| FunctionCall + FunctionCallOutput | ✅ | |
| Thinking block (non-stream) | ✅ | ProviderData restores signature |
| Thinking block (stream) | ⛔ | Signature lost; ProviderData nil |
| `redacted_thinking` | ⛔ | Dropped both ways |
| `stop_sequence` matched string | ⛔ | Dropped |
| `disable_parallel_tool_use` | ⛔ | Not parsed inbound, not emitted outbound |
| `OutputConfig.Format` | ⛔ | Not emitted |
| Sampling (temp/topP/topK/stop) | ✅ | |
| Seed / FrequencyPenalty / PresencePenalty | ⛔ | Dropped outbound |
| Cache breakpoints | ⚠️ | Emitted outbound; inbound markers dropped by design |
| Image in tool_result | ⛔ | Dropped |
| FilePart.Filename | ⛔ | Dropped |
| Citation annotations in stream | ⛔ | Dropped on `content_block_stop` assembly |

### Anthropic → canonical → Anthropic

| Field | Round-trips? | Notes |
|---|---|---|
| Text content | ✅ | |
| Tool use + tool result | ✅ | |
| Thinking + signature (non-stream) | ✅ | |
| Thinking + signature (stream) | ⛔ | Signature lost |
| `redacted_thinking` | ⛔ | Dropped |
| `stop_reason` | ✅ | |
| `stop_sequence` matched string | ⛔ | Dropped at ParseResponse |
| Usage (input/output/cache) | ✅ | |
| `char_location`/`page_location` citations | ⛔ | Dropped |
| `url_citation` | ✅ | |
| `server_tool_use` | ⛔ | Dropped |
| Inbound `cache_control` markers | ⛔ | By design |

---

## Recommendations (prioritized)

### P0 — Correctness bugs

1. **Capture thinking signature in streaming path** (`handleContentBlockDelta` / `handleContentBlockStop`).
   Anthropic streams a `signature_delta` delta type that has no case in the switch at `translator_canonical.go:809`. Add a `"signature_delta"` case, accumulate in `anthropicStreamBlock`, and set `Reasoning.ProviderData` from it in `handleContentBlockStop`. Without this, multi-turn extended thinking continuation is silently broken in all streamed responses.

2. **Wire `ToolsConfig.Parallel` into `SerializeRequest`**.
   At `translator_canonical.go:350`, replace `canonicalToolChoiceToAnthropic(tc.Choice, nil)` with `canonicalToolChoiceToAnthropic(tc.Choice, tc.Parallel)`. The helper already handles the bool — the call site simply never passes it.

### P1 — Silent data loss worth surfacing

3. **Surface `stop_sequence` matched string**.
   The `anthropicFullResp.StopSeq` field is parsed (`translator_canonical.go:112`) but never forwarded. Store it in `Response.Extensions["stop_sequence"]` or a dedicated canonical field, and do the same for the stream path (`message_delta.delta.stop_sequence`). Callers doing conditional logic on which stop sequence fired currently have no way to know.

4. **Log (warn) or error on `redacted_thinking` blocks**.
   Silent drop means callers building extended-thinking applications have no observable signal that redacted blocks were received. At minimum, increment a Prometheus counter or return the block index in `Response.Extensions`.

5. **`max_tokens` default: log or surface**.
   When `SamplingParams.MaxTokens` is nil, the adapter silently caps at 4096. For models with larger windows (claude-3-7-sonnet, extended-thinking mode), this can truncate responses without the caller realising. Consider logging at warn level or populating `Response.IncompleteDetails` when `stop_reason=max_tokens` and the cap came from the default.

### P2 — Missing features

6. **Implement `OutputConfig.Format`** (structured output / JSON mode).
   Anthropic supports `{"type": "json_object"}` via the `response_format` extension or, for Claude 3 models, it is triggered via a system prompt workaround. At minimum, return an explicit error when `Output.Format` is set rather than silently ignoring it.

7. **Parse and forward `tool_choice.disable_parallel_tool_use` in `ParseRequest`**.
   `anthropicParseToolChoice` at `translator_canonical.go:1228` only reads `type`/`name`. Add reading of `disable_parallel_tool_use` and populate `ToolsConfig.Parallel`.

8. **Preserve citation annotations through stream `item.completed`**.
   The `handleContentBlockStop` assembles the `OutputTextPart` without annotations (`translator_canonical.go:848–850`). Anthropic may stream `content_block_delta` events with `citations_delta` in future; currently the sync path preserves annotations but the stream path doesn't assemble them.
