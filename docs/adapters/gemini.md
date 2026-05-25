# Google Gemini (generateContent) — Canonical Fidelity Audit

> Audited 2026-05-25 against `pkg/adapters/gemini/translator_canonical.go`. Critical, honest map. ADAPTER IS NEW (PR #200), UPSTREAM-ONLY, AND NOT YET DOGFOODED — treat gaps as bugs until proven otherwise.

---

## Verdict

The adapter covers the happy path (text, tool call, reasoning, multimodal, sampling) correctly and the canonical ↔ Gemini conversion is structurally sound. However it has **four correctness bugs that will surface on real traffic**:

1. **Parallel tool-call CallID collision** — two calls to the same function in one response get identical `call_id` values; a downstream `FunctionCallOutput` will match both or neither.
2. **`Output.Format` (structured output) is completely silently dropped** — `responseMimeType`/`responseSchema` are never written; JSON mode requests will get plain text back from Gemini with no error.
3. **`CacheConfig` and `ItemCacheConfig.Anchor` are silently discarded** — callers that set cache anchors get no cache hits and no feedback.
4. **Arguments delta in `NewFromCanonicalStream`** wraps partial JSON in `%q` (double-quoted string), producing invalid JSON in the `args` object field of the Gemini SSE frame.

---

## Request: canonical → Gemini (`SerializeRequest`)

| Canonical element | Status | Notes + file:line |
|---|---|---|
| `Instructions` → `systemInstruction` | ✅ | `translator_canonical.go:159–164` |
| `Message{Role: user}` → `contents[].role=user` | ✅ | `translator_canonical.go:964–969` |
| `Message{Role: assistant}` → `contents[].role=model` | ✅ | `translator_canonical.go:971–976` |
| `Message{Role: system\|developer}` → appended to `systemInstruction` | ✅ | `translator_canonical.go:950–961` |
| `TextPart` → `geminiPart{Text}` | ✅ | `translator_canonical.go:1002–1003` |
| `OutputTextPart` → `geminiPart{Text}` | ✅ | `translator_canonical.go:1004–1005` |
| `ImagePart` (data: URL) → `inlineData{mimeType, data}` | ✅ | `translator_canonical.go:1031–1043` |
| `ImagePart` (plain URL) → `fileData{fileUri}` | ✅ | `translator_canonical.go:1042–1043` |
| `ImagePart.Detail` | ⛔ | Field read nowhere; `Detail` is a canonical-only hint for OpenAI. Gemini has no equivalent — acceptable to drop, but should be documented. |
| `FilePart.FileData` (base64) → `inlineData` | ✅ | `translator_canonical.go:1013–1018` |
| `FilePart.FileURL` → `fileData{fileUri}` | ✅ | `translator_canonical.go:1019–1021` |
| `FilePart.FileID` | ⛔ | Never read. If set without `FileData`/`FileURL` the part falls through to the error branch at `translator_canonical.go:1022–1024`. |
| `FilePart.Filename` | ⛔ | Silently dropped. Gemini has no `displayName` equivalent per part, so this is expected — but undocumented. |
| `FunctionCall` → `geminiPart{functionCall}` | ✅ | `translator_canonical.go:893–908` (via `pendingFCs`) |
| `FunctionCallOutput` → `geminiPart{functionResponse}` | ✅ | `translator_canonical.go:911–939` |
| `FunctionCallOutput.Content[]` (part-array form) | ⚠️ | Only `*v1.TextPart` is handled; other part types in `Content` are silently skipped (`translator_canonical.go:918–925`). |
| `Reasoning` item in input | 🔒 | Explicitly dropped with comment "Gemini manages its own thinking" (`translator_canonical.go:987–989`). Correct per Gemini semantics, but caller receives no feedback. |
| `SamplingParams.Temperature` → `generationConfig.temperature` | ✅ | `translator_canonical.go:170` |
| `SamplingParams.TopP` → `generationConfig.topP` | ✅ | `translator_canonical.go:171` |
| `SamplingParams.TopK` → `generationConfig.topK` | ✅ | `translator_canonical.go:172` |
| `SamplingParams.MaxTokens` → `generationConfig.maxOutputTokens` | ✅ | `translator_canonical.go:173` |
| `SamplingParams.Stop` → `generationConfig.stopSequences` | ✅ | `translator_canonical.go:174` |
| `SamplingParams.Seed` → `generationConfig.seed` | ✅ | `translator_canonical.go:175` |
| `SamplingParams.FrequencyPenalty` → `generationConfig.frequencyPenalty` | ✅ | `translator_canonical.go:176` |
| `SamplingParams.PresencePenalty` → `generationConfig.presencePenalty` | ✅ | `translator_canonical.go:177` |
| `ReasoningConfig.BudgetTokens` → `thinkingConfig.thinkingBudget` | ✅ | `translator_canonical.go:180–186` |
| `ReasoningConfig.Effort` | ⛔ | Never read. Gemini's `thinkingBudget` is the only control — `Effort` string is silently lost. |
| `ReasoningConfig.Summary` | ⛔ | Never read; silently dropped. |
| `ToolsConfig.Definitions` → `tools[].functionDeclarations` | ✅ | `translator_canonical.go:192–213` |
| `ToolsConfig.Choice` → `toolConfig.functionCallingConfig` | ✅ | `translator_canonical.go:211–213` |
| `ToolsConfig.Parallel` | ⛔ | Never read; silently dropped. Gemini has no equivalent. |
| `OutputConfig.Format` (json_object / json_schema) | ⛔ | **Never read.** `responseMimeType` and `responseSchema` are never set. A caller requesting structured output gets plain text with no error. `translator_canonical.go:166–215` — `opts.Output` is never accessed. |
| `OutputConfig.Verbosity` | ⛔ | Never read; silently dropped. |
| `CacheConfig.Instructions` / `.Tools` | ⛔ | Never read. Gemini's explicit context caching uses a separate API (`cachedContent` resource); inline breakpoints don't exist. Dropping is correct but callers receive no feedback. |
| `Message.CacheConfig` / `ItemCacheConfig.Anchor` | ⛔ | Never read; silently dropped at all call sites. |
| `Message.ProviderData` | ⛔ | Never read; silently dropped. |
| `FunctionCall.ProviderData` | ⛔ | Never read; silently dropped. |
| `Reasoning.ProviderData` | ⛔ | Never read; silently dropped. |
| `Request.User` | ⛔ | Never read; silently dropped. Gemini has no equivalent wire field. |
| `Request.Metadata` | ⛔ | Never read; silently dropped. |
| `Request.Extensions` | ⛔ | Never read; silently dropped. |
| `Request.OutputMode` | 🔒 | Not read here — routing concern handled by `Spec.UpstreamPathFn`. Correct for an upstream-only adapter. |
| ModelOpts resolution (`resolveModelOpts`) | ⚠️ | Falls back to the single entry when `ModelConfig` has exactly one key regardless of model name match (`translator_canonical.go:1128–1138`). This silently applies unrelated model opts if the caller has one entry keyed to a different model name. |

---

## Request: Gemini → canonical (`ParseRequest`)

| Gemini element | Status | Notes + file:line |
|---|---|---|
| `contents[].role=user` → `Message{Role: user}` | ✅ | `translator_canonical.go:813–840` |
| `contents[].role=model` → `Message{Role: assistant}` | ✅ | `translator_canonical.go:843–866` |
| Unknown roles | 🔒 | Silently treated as `user` (`translator_canonical.go:867–877`). No error. |
| `systemInstruction` → `Instructions` | ✅ | `translator_canonical.go:40–42` |
| `systemInstruction` multi-part | ✅ | All text parts joined with newline (`translator_canonical.go:787–803`). Non-text parts in `systemInstruction` are silently dropped. |
| `generationConfig.temperature/topP/topK/maxOutputTokens/stopSequences/seed/frequencyPenalty/presencePenalty` | ✅ | `translator_canonical.go:55–88` |
| `generationConfig.thinkingConfig.thinkingBudget` → `ReasoningConfig.BudgetTokens` | ✅ | `translator_canonical.go:91–95` |
| `generationConfig.thinkingConfig.includeThoughts` | ⛔ | Never read on parse; lost. |
| `generationConfig.candidateCount` | ⛔ | Present in struct (`types.go:77`) but never parsed into canonical — silently dropped. |
| `generationConfig.responseMimeType` / `responseSchema` | ⛔ | Present in struct (`types.go:78–79`) but never mapped to `OutputConfig.Format` — silently dropped. |
| `tools[].functionDeclarations` → `ToolsConfig.Definitions` | ✅ | `translator_canonical.go:100–115` |
| `toolConfig.functionCallingConfig.mode` → `ToolChoice.Mode` | ✅ | `translator_canonical.go:117–123` |
| `toolConfig.functionCallingConfig.allowedFunctionNames` with >1 name | ⚠️ | Only `len==1` maps to `{mode:"function", FunctionName:...}`; `len>1` collapses to `{mode:"required"}`, losing the function name list (`translator_canonical.go:1053–1057`). |
| `inlineData` → `ImagePart` (data: URL) | ✅ | `translator_canonical.go:829–831` |
| `fileData` → `FilePart{FileURL, MediaType}` | ✅ | `translator_canonical.go:832–835` |
| `functionCall` in user content | ⛔ | The user-role parsing loop has no `p.FunctionCall` branch; a `functionCall` part in a `user` content block is silently dropped (`translator_canonical.go:816–838`). In practice Gemini never sends this, but the asymmetry is fragile. |
| `functionResponse` in model content | ⛔ | The model-role parsing loop has no `p.FunctionResponse` branch; silently dropped (`translator_canonical.go:843–865`). |
| `model` field (convenience) | ✅ | `translator_canonical.go:35–37` |
| `safetySettings` | ⛔ | Not in the parse struct; silently ignored on parse. |
| `cachedContent` | ⛔ | Not in the parse struct; silently ignored on parse. |

---

## Response: Gemini → canonical (`ParseResponse`)

| Gemini element | Status | Notes + file:line |
|---|---|---|
| `candidates[0].content.parts[].text` (non-thought) → `Message` | ✅ | `translator_canonical.go:294–303` |
| `candidates[0].content.parts[].text` + `thought=true` → `Reasoning` | ✅ | `translator_canonical.go:285–292` |
| `candidates[0].content.parts[].functionCall` → `FunctionCall` | ✅ | `translator_canonical.go:259–281` |
| `FunctionCall.CallID` | ⚠️ | Set to `p.FunctionCall.Name` (`translator_canonical.go:279`). When the same function is called twice in parallel, both `FunctionCall` items share the same `CallID`. A subsequent `FunctionCallOutput` routed by `CallID` will be ambiguous. |
| `FunctionCall.ID` | 🔒 | Synthesized as `fmt.Sprintf("fc_%d", outputIndex)` (`translator_canonical.go:277`). Sequential across the response; not globally unique across requests. |
| `Message.ID` | 🔒 | Synthesized as `fmt.Sprintf("msg_%d", outputIndex)` (`translator_canonical.go:295`). Same non-uniqueness. |
| `Reasoning.ID` | 🔒 | Synthesized as `fmt.Sprintf("rs_%d", outputIndex)` (`translator_canonical.go:287`). Same. |
| `Response.ID` | 🔒 | `fmt.Sprintf("gemini-%d", time.Now().UnixNano())` (`translator_canonical.go:245`). Non-deterministic; two calls in the same nanosecond collide on some platforms. |
| `Response.CreatedAt` | 🔒 | `time.Now().Unix()` (`translator_canonical.go:247`). Relay wall-clock, not Gemini-stamped. |
| `Response.Model` | ✅ | Mapped from `gr.ModelVersion` (`translator_canonical.go:248`). |
| `finishReason: STOP` → `completed/stop` | ✅ | `translator_canonical.go:1097` |
| `finishReason: MAX_TOKENS` → `incomplete/length` | ✅ | `translator_canonical.go:1099` |
| `finishReason: SAFETY` → `completed/content_filter` | ✅ | `translator_canonical.go:1101` |
| `finishReason: RECITATION` → `completed/content_filter` | ✅ | `translator_canonical.go:1101` (grouped with SAFETY) |
| `finishReason: BLOCKLIST` | ⚠️ | Falls through to default `completed/stop` (`translator_canonical.go:1103`). Should be `content_filter`. |
| `finishReason: PROHIBITED_CONTENT` | ⚠️ | Falls through to default `completed/stop`. Should be `content_filter`. |
| `finishReason: SPII` | ⚠️ | Falls through to default `completed/stop`. Should be `content_filter`. |
| `finishReason: MALFORMED_FUNCTION_CALL` | ⚠️ | Falls through to default `completed/stop`. No canonical equivalent — arguably `failed` or a new reason. Currently silently misreported as `stop`. |
| `finishReason: LANGUAGE` | ⚠️ | Falls through to default `completed/stop`. |
| `hasFunctionCall=true` overrides `finishReason` check | ✅ | Correctly sets `tool_calls` regardless of Gemini's `STOP` (`translator_canonical.go:1091–1093`). |
| `usageMetadata.promptTokenCount` → `Usage["input"]` | ✅ | `tokens.go:35` |
| `usageMetadata.candidatesTokenCount` → `Usage["output"]` | ✅ | `tokens.go:38` |
| `usageMetadata.cachedContentTokenCount` → `Usage["cache_read"]` | ✅ | `tokens.go:41` |
| `usageMetadata.thoughtsTokenCount` → `Usage["reasoning"]` | ✅ | `tokens.go:44` |
| `Usage["cache_creation"]` | ⛔ | Gemini has no cache-creation count (context cache creation is a separate billing API call). Key will never appear. Consumers that compute net cost from `cache_creation` see zero — correct but asymmetric vs Anthropic. |
| `candidates[1..n]` (multiple candidates) | ⛔ | Only `candidates[0]` is ever read (`translator_canonical.go:252`). `candidateCount>1` requests silently discard all but the first candidate. |
| Grounding metadata / `safetyRatings` | ⛔ | Not in response struct; silently discarded. |
| `Response.Extensions` | ⛔ | Never populated. Gemini grounding, search metadata, attribution all lost. |

---

## Response: canonical → Gemini (`SerializeResponse`)

| Canonical element | Status | Notes + file:line |
|---|---|---|
| `Message` → `geminiPart{Text}` | ✅ | `translator_canonical.go:323–330` |
| `FunctionCall` → `geminiPart{functionCall}` | ✅ | `translator_canonical.go:332–342` |
| `Reasoning` → `geminiPart{Text, Thought:true}` | ✅ | `translator_canonical.go:343–349` |
| `FinishReason` → `finishReason` | ✅ | `translator_canonical.go:319` |
| `IncompleteDetails` checked before `FinishReason` | ✅ | `translator_canonical.go:1109–1111` |
| `FinishReasonToolCalls` → Gemini `STOP` | 🔒 | `translator_canonical.go:1119–1120`. Gemini uses `STOP` here; documented in comment. |
| `FinishReasonRefusal` | ⚠️ | Falls through to default `STOP` (`translator_canonical.go:1121–1123`). Gemini has no refusal reason; `STOP` is the best available mapping but is undocumented. |
| `Usage` → `usageMetadata` | ✅ | `translator_canonical.go:363–378` |
| `Response.Model` → `modelVersion` | ✅ | `translator_canonical.go:380–382` |
| `Response.ID` | ⛔ | Not emitted. Gemini response shape has no `responseId` field at the top level. Correct omission. |
| `Response.Extensions` | ⛔ | Never read; silently dropped. |
| `Response.Error` | ⛔ | Never serialized. If a failed canonical response is passed to `SerializeResponse`, the error field disappears entirely. |
| All item types besides `Message`, `FunctionCall`, `Reasoning` | ⛔ | `FunctionCallOutput` in output (unusual but valid) is silently skipped by the type switch (`translator_canonical.go:321–351`). |

---

## Streaming

### To canonical (`NewToCanonicalStream`)

| Aspect | Status | Notes + file:line |
|---|---|---|
| `generation.created` on first frame | ✅ | `translator_canonical.go:455–467` |
| `item.started` + `item.delta` + `item.completed` for text | ✅ | `translator_canonical.go:549–575` |
| `item.started` + `item.delta` + `item.completed` for reasoning | ✅ | `translator_canonical.go:521–547` |
| `item.started` + `item.delta` + `item.completed` for function calls | ✅ | `translator_canonical.go:483–519` |
| `generation.completed` with usage | ✅ | `translator_canonical.go:580–583` |
| Terminal frame with no content (usage-only frame) | ✅ | `translator_canonical.go:474–478` |
| `error` event on Gemini error shape | ✅ | `translator_canonical.go:436–449` |
| `FunctionCall.CallID` collision in stream | ⚠️ | Same bug as `ParseResponse`: `CallID` set to function name (`translator_canonical.go:604`). |
| `sawFunctionCall` overrides `finishReason` in `emitCompletion` | ✅ | `translator_canonical.go:631` |
| `finishReason` fall-through (BLOCKLIST, etc.) | ⚠️ | Same incomplete mapping as `ParseResponse`; stream uses the same `geminiFinishReasonToCanonical` helper (`translator_canonical.go:631`). |
| Multiple text parts spanning multiple frames → one `Message` item | ✅ | `textBuf` accumulates; `closeCurrentItem` fires on type switch or terminal frame. |
| Item type switch mid-stream (text → reasoning → text) | ⚠️ | Switching from reasoning back to text opens a new `msg_N` item, but the prior text item was already closed. Two separate `Message` items are emitted. This is structurally correct but the second message has a higher index than the reasoning item — consumers accumulating a response array must handle non-sequential role switches. |

### From canonical (`NewFromCanonicalStream`)

| Aspect | Status | Notes + file:line |
|---|---|---|
| `generation.created` → nil (no Gemini open frame) | ✅ | `translator_canonical.go:663–667` |
| `item.started` → nil | ✅ | `translator_canonical.go:669–671` |
| `item.delta` text → Gemini frame with text part | ✅ | `translator_canonical.go:682–683` |
| `item.delta` reasoning → Gemini frame with `thought:true` part | ✅ | `translator_canonical.go:684–685` |
| `item.delta` arguments → Gemini frame with functionCall part | ⛔ | **Bug.** `json.RawMessage(fmt.Sprintf("%q", e.Delta))` (`translator_canonical.go:688`) wraps the partial JSON string in Go's `%q` quoting, producing a JSON-encoded string (e.g. `"{\""key\""}"`) where the wire expects a JSON object. The resulting Gemini SSE frame has an invalid `args` value. |
| `item.completed` → nil | ✅ | `translator_canonical.go:701–703` |
| `generation.completed` → terminal Gemini frame with `finishReason` + usage | ✅ | `translator_canonical.go:705–739` |
| `error` → Gemini error frame | ✅ | `translator_canonical.go:741–754` |
| `FunctionCall.Name` missing in arguments delta frame | ⚠️ | The `geminiFC` struct emitted for an arguments delta has an empty `Name` field (`translator_canonical.go:688`). A streaming Gemini client reading deltas incrementally will see a `functionCall` part with no `name`. The name only appears on `item.started` which is translated to nil. |

---

## Gemini features with NO canonical representation

| Feature | Status | Details |
|---|---|---|
| `safetySettings` | ⛔ never read | Not in `geminiRequest` struct (`types.go:15–21`). A caller wanting to configure Gemini safety thresholds has no path — not even via `Extensions`. |
| `cachedContent` (context cache resource name) | ⛔ never read | Not in struct. The only way to use Gemini's explicit caching API (separate from `cachedContentTokenCount` in usage) is unavailable. |
| `generationConfig.responseMimeType` / `responseSchema` | ⛔ accepted in parse struct, never serialized | Struct fields exist (`types.go:78–79`) but `SerializeRequest` never writes them from `OutputConfig.Format`. |
| `generationConfig.candidateCount` | ⛔ accepted in parse struct, never produced | Field `types.go:77`. Always produces/consumes only `candidates[0]`. |
| `responseModalities` | ⛔ not in struct | Image/audio output modalities unavailable. |
| `grounding` / search tool | ⛔ not in struct | Gemini built-in tools (Google Search, code execution) have no canonical or extension mapping. |
| `safetyRatings` on candidate | ⛔ discarded on parse | `candidate` struct has no `safetyRatings` field. |
| Grounding metadata on response | ⛔ discarded | No `groundingMetadata` field; not forwarded to `Extensions`. |
| `thoughtSignature` on reasoning parts (v1beta newer models) | ⛔ not in struct | `geminiPart` has no `thoughtSignature` field. Cannot round-trip Gemini's reasoning signature for same-vendor fidelity (no `ProviderData` populated). |
| `audio` input/output parts | ⛔ not in struct | No canonical mapping for audio modality exists either. |
| Multiple `AllowedFunctionNames` in `ANY` mode | ⚠️ data loss | Collapses to `required` losing the name list (`translator_canonical.go:1055–1057`). |

---

## ⚠️ Silently dropped (no error, no log)

1. `OutputConfig.Format` (json_object / json_schema) — `translator_canonical.go:166–215` (`opts.Output` never accessed in `SerializeRequest`)
2. `CacheConfig.Instructions` / `.Tools` — never read in `SerializeRequest`
3. `ItemCacheConfig.Anchor` on any item — never read anywhere
4. `Message.ProviderData`, `FunctionCall.ProviderData`, `Reasoning.ProviderData` — never read in `SerializeRequest`, never populated in `ParseResponse`
5. `ReasoningConfig.Effort` and `ReasoningConfig.Summary` — `translator_canonical.go:166–215`
6. `ToolsConfig.Parallel` — never read
7. `Request.User`, `Request.Metadata`, `Request.Extensions` — never read
8. `Response.Extensions` in `SerializeResponse` — `translator_canonical.go:317–385`
9. `Response.Error` in `SerializeResponse` — type switch on output items never handles the error case
10. Candidates beyond `candidates[0]` — `translator_canonical.go:252`
11. Gemini `safetyRatings`, `groundingMetadata`, `thoughtSignature` — not in wire structs
12. `FilePart.FileID` — falls to error only if no `FileData` or `FileURL`; error path is the only signal

---

## 🔒 Hardcoded / defaulted

1. `Response.ID` = `fmt.Sprintf("gemini-%d", time.Now().UnixNano())` — non-deterministic, not Gemini-issued (`translator_canonical.go:245`)
2. `Response.CreatedAt` = `time.Now().Unix()` — relay wall-clock, not provider timestamp (`translator_canonical.go:247`)
3. `Response.Object` = `"response"` — correct per canonical contract (`translator_canonical.go:246`)
4. `thinkingConfig.includeThoughts = true` always when `ReasoningConfig` is set — `translator_canonical.go:181`. Cannot request budget without requesting thought text.
5. `FunctionCall.CallID` = function name — Gemini provides no call ID (`translator_canonical.go:279`, `604`)
6. Function call `ID` = `fmt.Sprintf("fc_%d", outputIndex)` — local counter only (`translator_canonical.go:277`)
7. Missing `MediaType` on `FilePart` → `"application/octet-stream"` (`translator_canonical.go:1015–1016`)
8. Missing `Args` on `FunctionCall` → `"{}"` (`translator_canonical.go:273–275`)
9. `generationConfig` only emitted when at least one sampling field is non-nil or reasoning is set — callers relying on a `generationConfig: {}` sentinel will not get one

---

## ⛔ Unsupported (explicit error)

1. Non-`*v1.FunctionTool` tool type in `ToolsConfig.Definitions` → `fmt.Errorf("gemini serialize_request: unsupported tool type %T", tool)` — `translator_canonical.go:197`
2. `FilePart` with neither `FileData` nor `FileURL` → `fmt.Errorf("gemini serialize_request: file part has no data or URL")` — `translator_canonical.go:1022–1024`
3. Unknown `Part` type in `canonicalPartsToGemini` → `fmt.Errorf("gemini serialize_request: unsupported part type %T", p)` — `translator_canonical.go:1025–1026`
4. Malformed JSON in `ParseRequest` body → propagated error
5. Malformed `generationConfig` JSON → propagated error

---

## Known correctness risks (new, undogfooded)

### 1. Parallel tool-call `CallID` collision (HIGH)
`translator_canonical.go:279` and `604`. When Gemini returns two `functionCall` parts with the same function name (e.g., two `search` calls), both `FunctionCall` items carry `CallID = "search"`. A subsequent multi-turn request with `FunctionCallOutput{CallID: "search"}` is serialized via `geminiFR{Name: "search"}` — Gemini matches by name, not by an opaque ID, so functionally it may work for simple cases. But the canonical contract requires `CallID` to be unique within a response. Any canonical middleware or observer that tracks call→result pairs by `CallID` will corrupt state.

### 2. `Output.Format` silently ignored (HIGH)
`SerializeRequest` reads `opts.Sampling`, `opts.Reasoning`, `opts.Tools` but never `opts.Output` (`translator_canonical.go:169–214`). A caller setting `Format{Type:"json_object"}` will receive whatever text the model decides to produce. No error is returned.

### 3. Arguments delta wraps in `%q` producing invalid JSON (HIGH)
`NewFromCanonicalStream`, `translator_canonical.go:688`:
```go
p = geminiPart{FunctionCall: &geminiFC{Args: json.RawMessage(fmt.Sprintf("%q", e.Delta))}}
```
`fmt.Sprintf("%q", e.Delta)` produces a Go-quoted string literal (e.g. `"{\"q\":\"foo\"}"`) not a JSON object. The Gemini SSE frame will carry `"args": "{\"q\":\"foo\"}"` (a string) instead of `"args": {"q":"foo"}` (an object). Gemini streaming clients reading this will fail to decode the function call arguments.

### 4. `finishReason` fall-through for safety sub-reasons (MEDIUM)
`BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `MALFORMED_FUNCTION_CALL`, `LANGUAGE` all map to `completed/stop` (`translator_canonical.go:1103`). A blocked response looks identical to a normal completion. Downstream safety auditing, billing checks, and retry logic will misclassify these.

### 5. `resolveModelOpts` false match (LOW-MEDIUM)
`translator_canonical.go:1128–1138`: if `ModelConfig` has exactly one entry keyed to model A, and `SerializeRequest` is called for model B (or no model), that entry is used. In the upstream-only path the model key is typically the relay model name, not necessarily the Gemini model name, so this may silently apply opts from the wrong model entry.

### 6. `generation.created` response ID is time-based (LOW)
`translator_canonical.go:458–459`: `s.responseID = fmt.Sprintf("gemini-%d", s.created)` uses Unix seconds, not nanoseconds, so two streams starting in the same second share an ID. `ParseResponse` uses nanoseconds which is better but still not collision-proof under concurrent load.

### 7. `NewFromCanonicalStream`: function call name absent in delta frame (MEDIUM)
`translator_canonical.go:686–689`: the `geminiFC` emitted for an argument delta has `Name: ""`. A streaming Gemini client accumulating parts incrementally will receive a `functionCall` part without a function name until — or unless — it re-reads the accumulated item from a `item.completed` event it will never see (translated to nil).

---

## Round-trip fidelity

### canonical → Gemini → canonical
- Text, sampling, tool definitions, tool choice, reasoning budget: **lossless**
- `OutputConfig.Format`: **lost** (never written to Gemini request)
- `CacheConfig` / `ItemCacheConfig`: **lost**
- `ProviderData` on any item: **lost**
- `User`, `Metadata`, `Extensions`: **lost**
- `ReasoningConfig.Effort` / `.Summary`: **lost**
- `FunctionCallOutput.Content[]` non-text parts: **lost**

### Gemini → canonical → Gemini
- Text content, sampling params, tool declarations: **lossless**
- `responseSchema` / `responseMimeType`: **lost** (parsed into struct fields, never surfaced in canonical, never re-emitted)
- `candidateCount`: **lost** (struct field, never canonical)
- `safetySettings`: **lost** (not in struct)
- `cachedContent`: **lost** (not in struct)
- `thoughtSignature`: **lost** (not in struct)
- `grounding` tools / metadata: **lost**

---

## Recommendations (prioritized)

1. **(P0) Fix arguments delta encoding in `NewFromCanonicalStream`** (`translator_canonical.go:688`). Replace `json.RawMessage(fmt.Sprintf("%q", e.Delta))` with logic that either passes the raw delta bytes directly (`json.RawMessage(e.Delta)`) or emits a partial-accumulation frame. Also populate `FunctionCall.Name` in the delta frame so streaming Gemini clients can identify which function is being called.

2. **(P0) Map `OutputConfig.Format` to `responseMimeType`/`responseSchema` in `SerializeRequest`**. When `Format.Type` is `"json_object"` set `responseMimeType = "application/json"`. When `"json_schema"` set both `responseMimeType` and `responseSchema`. Return an error rather than silently producing plain text.

3. **(P1) Expand `geminiFinishReasonToCanonical`** to cover `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII` → `content_filter`; `MALFORMED_FUNCTION_CALL` → consider `failed` with an `IncompleteDetails` reason. This affects both `ParseResponse` and `NewToCanonicalStream` (shared helper).

4. **(P1) Document `CallID` synthesis** in a code comment and add a test with two parallel calls to the same function. If the relay's `FunctionCallOutput` matching is name-based rather than ID-based for Gemini, document it. If it must be unique, append the output index: `CallID = fmt.Sprintf("%s_%d", name, outputIndex)`.

5. **(P2) Add `thoughtSignature` to `geminiPart` and round-trip it through `ProviderData`** on `Reasoning` items. This preserves Gemini's reasoning signature for same-vendor multi-turn correctness and matches the `provider_data` contract in the canonical protocol.

6. **(P2) Surface `CacheConfig` drop as a no-op comment** rather than silent discard. Optionally log a debug line; this prevents hours of debugging why cache hits never appear on Gemini routes.

7. **(P3) Add `safetySettings` to `geminiRequest` struct** and map it from `Request.Extensions["gemini.safety_settings"]` so callers can pass safety thresholds without a protocol change.

8. **(P3) Tighten `resolveModelOpts`** — the single-entry fallback (`translator_canonical.go:1130–1135`) should only fire when the key is `"*"`, not for any arbitrary single entry. Add a test with two different model names.
