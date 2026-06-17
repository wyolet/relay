# Anthropic API — Relay Integration Reference

> Snapshot date: 2026-05-07
> Sources:
> - https://platform.claude.com/docs/en/api/messages
> - https://platform.claude.com/docs/en/api/messages-streaming
> - https://platform.claude.com/docs/en/api/errors
> - https://platform.claude.com/docs/en/release-notes/api
> - https://platform.claude.com/docs/en/about-claude/models/overview
> - https://platform.claude.com/docs/en/api/openai-sdk
> - Internal: `internal/provider/anthropic/client.go`, `internal/api/anthropic/parse.go`
>
> Status: provider/API reference (snapshot 2026-05-07; updated as the
> Anthropic API evolves).

---

## 1. Endpoints We Care About

| Method | Path | Purpose | Status |
|--------|------|---------|--------|
| POST | `/v1/messages` | Chat completions (non-streaming) | GA |
| POST | `/v1/messages` (stream:true) | Chat completions (SSE streaming) | GA |
| POST | `/v1/messages/count_tokens` | Pre-flight token counting | GA (Dec 2024) |
| POST | `/v1/messages/batches` | Async batch processing at 50% cost | GA (Dec 2024); max_tokens cap 300k w/ `output-300k-2026-03-24` beta |
| POST | `/v1/complete` | Legacy text completions | **Deprecated** — use `/v1/messages` |
| POST | `/v1/chat/completions` | OpenAI-compatible shim (base: `https://api.anthropic.com/v1/`) | GA (Feb 2025); not feature-complete |

**OpenAI-compat limitations:** no prompt caching, no response_format/JSON schema, no audio, system messages are hoisted+concatenated, `strict` on tools ignored, extended thinking available via `extra_body` but thinking blocks not returned. Rate limits are same as `/v1/messages`.

---

## 2. Auth

| Header | Value | Notes |
|--------|-------|-------|
| `x-api-key` | `sk-ant-...` | Primary auth header. **Not** `Authorization: Bearer`. |
| `anthropic-version` | `2023-06-01` | Required. Only one supported value currently. |
| `anthropic-beta` | See table below | Optional. Comma-separate multiple values. |

**Current `anthropic-beta` values:**

| Value | Enables | Status |
|-------|---------|--------|
| `interleaved-thinking-2025-05-14` | Thinking blocks between tool calls | Beta |
| `computer-use-2025-01-24` | `computer_20250124` tool + new commands | Beta |
| `advisor-tool-2026-03-01` | Advisor model pairing for agentic tasks | Beta |
| `managed-agents-2026-04-01` | Managed agent sessions, memory, outcomes | Beta |
| `output-300k-2026-03-24` | 300k max_tokens on Batch API (Opus/Sonnet 4.6+) | Beta |
| ~~`prompt-caching-2024-07-31`~~ | Prompt caching | **GA** — no header needed since Dec 2024 |
| ~~`max-tokens-3-5-sonnet-2024-07-15`~~ | 8192 output tokens on Sonnet 3.5 | **GA** — retired |
| ~~`structured-outputs-2025-11-13`~~ | JSON schema outputs | **GA** since Jan 2026 |
| ~~`context-1m-2025-08-07`~~ | 1M token context on Sonnet 4/4.5 | **Retired** Apr 2026 |
| ~~`fine-grained-tool-streaming-2025-05-14`~~ | Unbuffered tool parameter streaming | **GA** since Feb 2026 |
| ~~`search-results-2025-06-09`~~ | Search result content blocks | **GA** since Aug 2025 |

---

## 3. Request Schema

### Required Fields

```json
{
  "model": "claude-opus-4-7",
  "max_tokens": 1024,
  "messages": [...]
}
```

`max_tokens`: 1–model-max (see §7). Set to `0` only for cache pre-warming (no output generated).

### `messages[]`

```
role: "user" | "assistant"
content: string | ContentBlock[]
```

### Content Block Types (current)

**Text block** (user or assistant):
```json
{ "type": "text", "text": "...",
  "cache_control": { "type": "ephemeral", "ttl": "5m" | "1h" },
  "citations": [...] }
```

**Image block** (user only):
```json
{ "type": "image",
  "source": { "type": "base64", "media_type": "image/jpeg|png|gif|webp", "data": "..." }
            | { "type": "url", "url": "https://..." },
  "cache_control": { "type": "ephemeral" } }
```

**Document block** (user only):
```json
{ "type": "document",
  "source": { "type": "base64", "media_type": "application/pdf", "data": "..." }
           | { "type": "text", "data": "...", "media_type": "text/plain" }
           | { "type": "url", "url": "https://..." }
           | { "type": "content", "content": string | ContentBlock[] },
  "title": "...", "context": "...",
  "citations": { "enabled": true },
  "cache_control": { "type": "ephemeral" } }
```

**Tool use block** (assistant response):
```json
{ "type": "tool_use", "id": "toolu_...", "name": "...", "input": {...} }
```

**Tool result block** (user, follows tool_use):
```json
{ "type": "tool_result", "tool_use_id": "toolu_...",
  "content": string | ContentBlock[], "is_error": false,
  "cache_control": { "type": "ephemeral" } }
```
Note: `cache_control` must be on the `tool_result` parent block, not on child blocks within `content` (enforced since May 2025).

**Thinking block** (assistant, extended thinking only):
```json
{ "type": "thinking", "thinking": "...", "signature": "..." }
```

**Redacted thinking block** (assistant, when thinking display is omitted):
```json
{ "type": "redacted_thinking", "data": "..." }
```

**Search result block** (user, GA Aug 2025):
```json
{ "type": "search_result", "title": "...", "source": "...",
  "content": [TextBlock], "cache_control": {...}, "citations": {...} }
```

### `system` Field

```
system: string | TextBlock[]
```
TextBlock array allows `cache_control` on individual blocks for prompt caching.

### Optional Top-Level Fields

| Field | Type | Range/Notes |
|-------|------|-------------|
| `system` | `string \| TextBlock[]` | System prompt. No "system" role in messages. |
| `temperature` | float | 0.0–1.0 (default 1.0). With extended thinking: 0.95–1.0. |
| `top_p` | float | Default 0.99 (changed May 2025 from 0.999). |
| `top_k` | int | Top-K sampling. |
| `stop_sequences` | `string[]` | Custom stop triggers. |
| `stream` | bool | SSE streaming. |
| `metadata.user_id` | string | UUID/hash for abuse detection; no PII. |
| `service_tier` | `"auto" \| "standard_only"` | Priority tier routing. |
| `inference_geo` | string | Geographic inference region. |

### `tools[]`

```json
{
  "name": "...", "description": "...",
  "input_schema": { "type": "object", "properties": {...}, "required": [...] },
  "cache_control": { "type": "ephemeral" },
  "strict": true,
  "defer_loading": true,
  "input_examples": [...]
}
```

### `tool_choice`

```json
{ "type": "auto" | "any" | "none", "disable_parallel_tool_use": false }
{ "type": "tool", "name": "fn_name", "disable_parallel_tool_use": false }
```

`tool_choice: none` added Feb 2025 — prevents Claude from calling any tool.

### Structured Outputs (GA Jan 2026)

```json
"output_config": {
  "format": { "type": "json_schema", "schema": {...} },
  "effort": "low" | "medium" | "high" | "xhigh" | "max"
}
```
No beta header required. Replaces the `output_format` field (deprecated).

### Extended Thinking

```json
"thinking": { "type": "enabled", "budget_tokens": 10000, "display": "summarized" | "omitted" }
           | { "type": "adaptive" }
           | { "type": "disabled" }
```

- `budget_tokens` ≥ 1024, < max_tokens
- `display: "omitted"` → thinking blocks have empty `thinking` field; `signature` still returned for multi-turn continuity
- `adaptive` (Opus 4.6+): model decides whether/how much to think
- When thinking enabled: `top_p` restricted to 0.95–1.0; `temperature` should not be set

### Prompt Caching

No beta header required (GA). Apply `cache_control: { "type": "ephemeral", "ttl": "5m" | "1h" }` to content blocks. The system caches the longest prefix matching a prior request. `1h` TTL is GA as of Aug 2025. Automatic caching available since Feb 2026 (single top-level `cache_control` on request body auto-advances the cache point).

---

## 4. Response Schema

### Non-Streaming Response

```json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "model": "claude-opus-4-7",
  "content": [ ContentBlock, ... ],
  "stop_reason": "end_turn" | "max_tokens" | "stop_sequence" | "tool_use" | "model_context_window_exceeded",
  "stop_sequence": null | "string",
  "usage": {
    "input_tokens": 100,
    "output_tokens": 50,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0,
    "server_tool_use": { "web_search_requests": 1 }
  }
}
```

`stop_reason` values:
- `end_turn` — natural completion
- `max_tokens` — hit max_tokens limit
- `stop_sequence` — matched a stop sequence
- `tool_use` — model wants to call a tool
- `model_context_window_exceeded` — context limit hit (added Sep 2025; allows requesting max tokens without calculating input size)

`usage` fields:
- `input_tokens` — tokens in the prompt (not counting cached reads)
- `output_tokens` — tokens generated
- `cache_creation_input_tokens` — tokens written to cache (billed at 1.25x)
- `cache_read_input_tokens` — tokens read from cache (billed at 0.1x)
- `server_tool_use` — server-side tool usage (e.g., `web_search_requests`)

**For Relay token accounting:** total billable input = `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`. Track them separately for analytics; cache creation and reads have different prices.

### Response `content[]` Block Types

| Type | When emitted |
|------|-------------|
| `text` | Normal response text |
| `tool_use` | Model wants to invoke a tool |
| `thinking` | Extended thinking block (when thinking enabled + display != omitted) |
| `redacted_thinking` | Thinking omitted per `display: "omitted"` |

---

## 5. Streaming Events

Response: `Content-Type: text/event-stream`. Each event is:
```
event: <type>
data: <json>
\n
```

### Event Sequence (guaranteed ordering)

```
message_start
  [ping ...]
  content_block_start (index: 0)
    content_block_delta ... (index: 0)
    [for thinking: signature_delta before stop]
  content_block_stop (index: 0)
  [more content blocks...]
message_delta
message_stop
```

### Event Payloads

**`message_start`**
```json
{ "type": "message_start",
  "message": { "id": "msg_...", "type": "message", "role": "assistant",
    "model": "claude-opus-4-7", "content": [], "stop_reason": null, "stop_sequence": null,
    "usage": { "input_tokens": 25, "output_tokens": 1,
               "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0 } } }
```

**`content_block_start`**
```json
{ "type": "content_block_start", "index": 0,
  "content_block": { "type": "text", "text": "" } }
```

**`content_block_delta`** — delta types:

| Delta type | Field | Use |
|-----------|-------|-----|
| `text_delta` | `text: string` | Text tokens |
| `input_json_delta` | `partial_json: string` | Tool input (partial JSON string) |
| `thinking_delta` | `thinking: string` | Thinking tokens |
| `signature_delta` | `signature: string` | Thinking signature (sent once before block_stop) |

**`content_block_stop`**
```json
{ "type": "content_block_stop", "index": 0 }
```

**`message_delta`**
```json
{ "type": "message_delta",
  "delta": { "stop_reason": "end_turn", "stop_sequence": null },
  "usage": { "output_tokens": 15,
             "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
             "server_tool_use": { "web_search_requests": 1 } } }
```
Note: `usage` in `message_delta` is **cumulative**, not incremental.

**`message_stop`**
```json
{ "type": "message_stop" }
```

**`ping`**
```json
{ "type": "ping" }
```
Sent periodically; no payload. Ignore.

**`error`** (mid-stream)
```json
{ "type": "error", "error": { "type": "overloaded_error", "message": "Overloaded" } }
```
Can occur after HTTP 200 has been returned. Handle separately from HTTP errors.

---

## 6. Errors

### HTTP Status Codes and Error Types

| HTTP | `error.type` | Meaning |
|------|-------------|---------|
| 400 | `invalid_request_error` | Bad format or content; also used for other 4xx not listed |
| 401 | `authentication_error` | API key issue |
| 402 | `billing_error` | Payment/billing problem |
| 403 | `permission_error` | Key lacks permission |
| 404 | `not_found_error` | Resource not found |
| 413 | `request_too_large` | Body > 32 MB (Messages); > 256 MB (Batch); > 500 MB (Files) |
| 429 | `rate_limit_error` | Rate limit or acceleration limit exceeded |
| 500 | `api_error` | Internal Anthropic error |
| 504 | `timeout_error` | Request timed out (use streaming for long requests) |
| 529 | `overloaded_error` | API temporarily overloaded |

### Error Envelope

```json
{
  "type": "error",
  "error": { "type": "not_found_error", "message": "The requested resource could not be found." },
  "request_id": "req_011CSHoEeqs5C35K2UUqR7Fy"
}
```

`request_id` added to error bodies Aug 2025 (also in `request-id` response header).

### Rate-Limit Response Headers

| Header | Meaning |
|--------|---------|
| `anthropic-ratelimit-requests-limit` | Request/min limit |
| `anthropic-ratelimit-requests-remaining` | Remaining requests |
| `anthropic-ratelimit-requests-reset` | Reset timestamp |
| `anthropic-ratelimit-tokens-limit` | Token/min limit |
| `anthropic-ratelimit-tokens-remaining` | Remaining tokens |
| `anthropic-ratelimit-tokens-reset` | Token reset timestamp |
| `anthropic-ratelimit-input-tokens-limit` | Input tokens/min |
| `anthropic-ratelimit-input-tokens-remaining` | Remaining input tokens |
| `anthropic-ratelimit-input-tokens-reset` | Input token reset |
| `anthropic-ratelimit-output-tokens-limit` | Output tokens/min |
| `anthropic-ratelimit-output-tokens-remaining` | Remaining output tokens |
| `anthropic-ratelimit-output-tokens-reset` | Output token reset |
| `retry-after` | Seconds to wait before retrying |
| `request-id` | Unique request ID for support |
| `anthropic-organization-id` | Org ID of the API key (added Feb 2026) |

---

## 7. Models

Current model IDs as of 2026-05-07:

### Current / Recommended

| Model ID | Description | Context | Max Output | Vision | Tools | Caching | Thinking | Batch |
|----------|-------------|---------|-----------|--------|-------|---------|---------|-------|
| `claude-opus-4-7` | Most capable, agentic coding | 1M | 128k | Yes | Yes | Yes | Adaptive | Yes |
| `claude-sonnet-4-6` | Speed + intelligence balance | 1M | 64k | Yes | Yes | Yes | Yes | Yes (300k w/beta) |
| `claude-haiku-4-5-20251001` | Fastest, near-frontier | 200k | 64k | Yes | Yes | Yes | Yes | Yes |

### Available Legacy Models

| Model ID | Notes |
|----------|-------|
| `claude-opus-4-6` | Predecessor to 4.7; adaptive thinking; 1M ctx |
| `claude-sonnet-4-5-20250929` | 200k ctx; extended thinking |
| `claude-opus-4-5-20251101` | 200k ctx; extended thinking |
| `claude-opus-4-1-20250805` | 200k ctx; extended thinking; no dual temp+top_p |
| `claude-sonnet-4-20250514` | **Deprecated** — retire Jun 15 2026 |
| `claude-opus-4-20250514` | **Deprecated** — retire Jun 15 2026 |

### Retired (requests now error)

`claude-3-haiku-20240307` (Apr 2026), `claude-3-7-sonnet-20250219` (Feb 2026), `claude-3-5-haiku-20241022` (Feb 2026), `claude-3-opus-20240229` (Jan 2026), Claude 2.x / Sonnet 3 / Claude 1 / Instant.

**All current models support:** text input, image input (vision), tool use, prompt caching, structured outputs.

**Note:** Claude Opus 4.1 does not allow both `temperature` and `top_p` set simultaneously.

**Note:** Opus 4.6+, Sonnet 4.6, Opus 4.7 do not support assistant message prefilling.

---

## 8. Capabilities Matrix

| Capability | Request surface | Response surface | Notes |
|-----------|----------------|-----------------|-------|
| Tool use | `tools[]`, `tool_choice` | `content[].type == "tool_use"`, `stop_reason == "tool_use"` | GA. `tool_choice: "none"` added Feb 2025 |
| Prompt caching | `cache_control: {type: "ephemeral", ttl: "5m"\|"1h"}` on blocks | `usage.cache_creation_input_tokens`, `usage.cache_read_input_tokens` | GA, no beta header |
| Extended thinking | `thinking: {type: "enabled"\|"adaptive", budget_tokens: N}` | `content[].type == "thinking"\|"redacted_thinking"` | Built-in to Claude 4.x models |
| Vision | `content[].type == "image"` (base64 or URL) | Normal text response | All current models |
| Structured outputs | `output_config.format.type == "json_schema"` | Guaranteed schema-conformant text | GA Jan 2026; no beta header |
| Computer use | `anthropic-beta: computer-use-2025-01-24` + `computer_20250124` tool | Tool use blocks | Beta |
| Batch API | `POST /v1/messages/batches` | Poll for results | GA. 50% cost discount |
| Token counting | `POST /v1/messages/count_tokens` | `{"input_tokens": N}` | GA |
| Streaming | `stream: true` | `text/event-stream` SSE | GA |
| Search results | `content[].type == "search_result"` in tool results | Citations in text | GA Aug 2025 |
| Citations | `citations` field on text/document blocks | `citations` in response text blocks | GA Jan 2025 |
| Files API | `POST /v1/files` (upload), reference in messages | — | GA May 2025 |
| Interleaved thinking | `anthropic-beta: interleaved-thinking-2025-05-14` | Thinking blocks between tool calls | Beta |

---

## 9. Implications for Relay

### Headers — Inbound Allowlist Gap

**Known limitation:** the inbound header allowlist (`pkg/httpheader`) does not include `anthropic-beta` or `anthropic-version`. Customers using beta features (extended thinking, computer use, interleaved thinking) send these headers, but they are stripped before the pipeline sees them — they need to be added to the inbound allowlist.

**Known limitation:** the outbound allowlist forwards `OpenAI-Beta` but not `anthropic-version` or `anthropic-beta`. The Anthropic client hardcodes `anthropic-version: 2023-06-01` (correct, still current), but `anthropic-beta` from the customer request is dropped and never forwarded, so a requested beta feature silently won't take effect.

### Token Accounting

Total billable input ≠ `input_tokens` alone. Full formula:
```
billed_input = input_tokens + cache_creation_input_tokens + cache_read_input_tokens
```
But each has a different multiplier: cache creation ~1.25x, cache reads ~0.1x vs normal 1x. Store all three fields separately in usage records. `server_tool_use.web_search_requests` is a separate cost item.

**Known limitation:** usage extraction does not separately meter all of `cache_creation_input_tokens` / `cache_read_input_tokens` / `server_tool_use` from the upstream response body. These fields are present upstream and are needed for fully accurate billing attribution.

### Streaming Passthrough

The tee/passthrough model works correctly for Anthropic SSE: the `event:` lines are plain bytes and pass through untouched. Confirm that nothing in the pipeline strips SSE `event:` name lines (only `data:` lines would be insufficient for correct client parsing).

### `anthropic-version` Pin

`client.go` hardcodes `anthropic-version: 2023-06-01`. This is the correct and only supported value. No change needed.

### Catalog Model Schema — Capability Flags Needed

Current `catalog.Model.Spec` needs the following flags for Anthropic models:
- `SupportsVision bool`
- `SupportsTools bool`
- `SupportsPromptCaching bool`
- `SupportsExtendedThinking bool`
- `SupportsBatchAPI bool`
- `SupportsStructuredOutputs bool`
- `MaxContextTokens int` (1M vs 200k varies by model)
- `MaxOutputTokens int` (32k / 64k / 128k varies)

### OpenAI-Compat Fallback Path (v2 Surface Area)

If Relay ever accepts OpenAI-shaped requests and forwards to Anthropic, translation points are:
- `messages[].role: "system"` → hoist to `system` field
- `response_format: { type: "json_object" }` → no direct equivalent; use `output_config.format.type: "json_schema"` (native only)
- `temperature` values > 1.0 → clamp to 1.0 (Anthropic range is 0–1, not 0–2)
- `n > 1` → not supported
- `logprobs`, `presence_penalty`, `frequency_penalty`, `seed` → silently ignored by Anthropic compat layer
- `max_completion_tokens` → map to `max_tokens`

---

## 10. Open Questions / Drift Watch

1. **`anthropic-beta` header forwarding**: Currently silently dropped. Until the outbound allowlist is updated, all beta features (extended thinking, computer use, interleaved thinking) are non-functional even if the customer sends the header.

2. **`stop_reason: "model_context_window_exceeded"`**: Added Sep 2025. Current parse/usage extraction code doesn't handle this. When this fires, `output_tokens` may be 0 or very low — ensure quota accounting handles it gracefully.

3. **Thinking block signature passthrough**: When using extended thinking in multi-turn conversations, the `signature` field of thinking blocks must be passed back verbatim in subsequent turns for the model to continue reasoning. If Relay ever deep-parses or rewrites the assistant message body, this will break multi-turn thinking.

4. **Automatic caching (Feb 2026)**: A single top-level `cache_control` on the request body auto-advances the cache point. No beta header. Relay passes the body through unchanged so this works transparently — but Relay's usage extraction needs to handle the resulting `cache_creation_input_tokens` correctly.

5. **`output-300k-2026-03-24` beta**: Enables 300k output on Batch API for Opus/Sonnet 4.6+. Currently not in the outbound allowlist.

6. **Models API (`GET /v1/models`)**: Returns `max_input_tokens`, `max_tokens`, and a `capabilities` object since Mar 2026. This should be used to populate the model catalog dynamically rather than hardcoding capability flags.

7. **`POST /v1/complete`** (legacy): Still reachable but deprecated. Any existing customer using it will get errors if Relay doesn't handle the path — confirm whether Relay has a route for this or returns an appropriate error.
