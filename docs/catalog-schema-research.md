# Catalog Schema Research — Provider + Model

> Snapshot date: 2026-05-07
> Sources:
> - https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json (fetched 2026-05-07)
> - https://docs.litellm.ai/docs/proxy/configs (fetched 2026-05-07)
> - https://openrouter.ai/api/v1/models (fetched 2026-05-07)
> - https://openrouter.ai/api/v1/providers (fetched 2026-05-07)
> - /Users/abror/projects/wyolet/relay/internal/catalog/types.go
> - /Users/abror/projects/wyolet/relay/docs/providers/anthropic.md
> - /Users/abror/projects/wyolet/relay/config/providers/openai/models/gpt-4o-mini.yaml
> - /Users/abror/projects/wyolet/relay/config/providers/ollama/models/gemma4.yaml
>
> Status: design reference for the schema PR

---

## 1. LiteLLM — What They Capture

**Source:** `https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json`
Approximately 415–420 model entries at time of fetch.

### model_info Field Inventory

| Field | Type | Example Value | Frequency | Description |
|-------|------|---------------|-----------|-------------|
| `litellm_provider` | string | `"anthropic"` | ~100% | Internal routing provider name |
| `mode` | string | `"chat"` | ~95% | `chat`, `completion`, `embedding`, `image_generation`, `audio_transcription`, `audio_speech`, `rerank` |
| `max_tokens` | int | `8192` | ~100% | Deprecated alias for max_output_tokens; kept for compat |
| `max_input_tokens` | int | `1000000` | ~95% | Max context/input tokens |
| `max_output_tokens` | int | `64000` | ~95% | Max generated output tokens |
| `input_cost_per_token` | float | `3e-06` | ~90% | USD per input token |
| `output_cost_per_token` | float | `1.5e-05` | ~90% | USD per output token |
| `input_cost_per_token_above_200k_tokens` | float | `6e-06` | ~15% | Tiered pricing above 200k (Gemini) |
| `output_cost_per_token_above_200k_tokens` | float | `3e-05` | ~15% | Tiered pricing above 200k |
| `input_cost_per_token_batches` | float | `1.5e-06` | ~20% | Batch API discount price |
| `output_cost_per_token_batches` | float | `7.5e-06` | ~20% | Batch API discount output |
| `cache_creation_input_token_cost` | float | `3.75e-06` | ~25% | Cost to write cache (Anthropic 1.25x) |
| `cache_creation_input_token_cost_above_1hr` | float | `7.5e-06` | ~15% | Cache write for 1h TTL tier |
| `cache_creation_input_token_cost_above_200k_tokens` | float | `7.5e-06` | ~10% | Cache write above 200k |
| `cache_creation_input_token_cost_above_1hr_above_200k_tokens` | float | `1.5e-05` | ~8% | Compound tier: 1h + 200k |
| `cache_read_input_token_cost` | float | `3e-07` | ~25% | Cache read (Anthropic ~0.1x) |
| `cache_read_input_token_cost_above_200k_tokens` | float | `6e-07` | ~10% | Cache read above 200k |
| `cache_creation_input_audio_token_cost` | float | varies | ~5% | Audio cache write |
| `cache_read_input_audio_token_cost` | float | varies | ~5% | Audio cache read |
| `output_cost_per_reasoning_token` | float | varies | ~10% | Reasoning token surcharge (o1/o3/R1) |
| `input_cost_per_audio_token` | float | varies | ~5% | Audio input token cost |
| `output_cost_per_audio_token` | float | varies | ~5% | Audio output token cost |
| `input_cost_per_image` | float | varies | ~5% | Per-image input cost |
| `input_cost_per_image_token` | float | varies | ~3% | Token-based image cost |
| `input_cost_per_pixel` | float | varies | ~3% | Pixel-based image cost |
| `input_cost_per_second` | float | varies | ~5% | Per-second audio/video |
| `input_cost_per_video_per_second` | float | varies | ~3% | Video per second |
| `output_cost_per_image` | float | varies | ~5% | Image generation base |
| `output_cost_per_image_above_512_and_512_pixels` | float | varies | ~3% | Image gen size tier |
| `output_cost_per_image_above_1024_and_1024_pixels` | float | varies | ~3% | Image gen size tier HD |
| `output_cost_per_image_premium_image` | float | varies | ~3% | Premium image gen |
| `output_cost_per_pixel` | float | varies | ~3% | Pixel-based image output |
| `output_cost_per_second` | float | varies | ~3% | Per-second audio output |
| `search_context_cost_per_query` | object | `{"search_context_size_high": 0.01, ...}` | ~5% | Web search pricing tiers |
| `file_search_cost_per_1k_calls` | float | varies | ~3% | File search tool cost |
| `file_search_cost_per_gb_per_day` | float | varies | ~3% | File storage cost |
| `code_interpreter_cost_per_session` | float | varies | ~3% | Code interpreter session |
| `vector_store_cost_per_gb_per_day` | float | varies | ~2% | Vector store |
| `computer_use_input_cost_per_1k_tokens` | float | varies | ~3% | Computer use input tokens |
| `computer_use_output_cost_per_1k_tokens` | float | varies | ~3% | Computer use output tokens |
| `audio_speech_cost_per_1k_tokens` | float | varies | ~3% | TTS output |
| `audio_transcription_cost_per_1k_tokens` | float | varies | ~3% | STT input |
| `output_vector_size` | int | varies | ~5% | Embedding output dimensions |
| `max_query_tokens` | int | varies | ~5% | Max query tokens (rerank) |
| `max_document_chunks_per_query` | int | varies | ~5% | Rerank docs per query |
| `max_tokens_per_document_chunk` | int | varies | ~5% | Rerank chunk size |
| `tool_use_system_prompt_tokens` | int | `159` | ~10% | Hidden system prompt token count for tools |
| `supported_endpoints` | array[string] | `["/chat/completions", "/messages/batches"]` | ~40% | Which API paths are valid |
| `supported_modalities` | array[string] | `["text", "image"]` | ~60% | Input modalities |
| `supported_output_modalities` | array[string] | `["text"]` | ~60% | Output modalities |
| `supported_regions` | array[string] | `["us-east-1", ...]` | ~15% | Available regions (Bedrock) |
| `supports_function_calling` | bool | `true` | ~70% | Tool/function call support |
| `supports_parallel_function_calling` | bool | `true` | ~50% | Parallel tool calls |
| `supports_vision` | bool | `true` | ~60% | Image input |
| `supports_image_input` | bool | `true` | ~60% | Image input (alias) |
| `supports_audio_input` | bool | `true` | ~10% | Audio input |
| `supports_audio_output` | bool | `true` | ~8% | Audio output |
| `supports_video_input` | bool | `true` | ~8% | Video input |
| `supports_pdf_input` | bool | `true` | ~30% | PDF/document input |
| `supports_embedding_image_input` | bool | `true` | ~5% | Image input for embeddings |
| `supports_prompt_caching` | bool | `true` | ~25% | Prompt caching |
| `supports_response_schema` | bool | `true` | ~50% | JSON schema output |
| `supports_native_structured_output` | bool | `true` | ~20% | Native structured output |
| `supports_system_messages` | bool | `true` | ~60% | System message support |
| `supports_tool_choice` | bool | `true` | ~60% | Tool choice control |
| `supports_reasoning` | bool | `true` | ~20% | Extended reasoning/thinking |
| `supports_max_reasoning_effort` | bool | `true` | ~10% | `reasoning_effort: high` |
| `supports_minimal_reasoning_effort` | bool | `true` | ~10% | `reasoning_effort: low` |
| `supports_none_reasoning_effort` | bool | `true` | ~5% | Disable reasoning |
| `supports_xhigh_reasoning_effort` | bool | `true` | ~5% | `reasoning_effort: xhigh` |
| `supports_assistant_prefill` | bool | `true` | ~20% | Assistant message prefill |
| `supports_native_streaming` | bool | `true` | ~40% | Native SSE streaming |
| `supports_web_search` | bool | `true` | ~10% | Built-in web search |
| `supports_computer_use` | bool | `true` | ~5% | Computer use tool |
| `deprecation_date` | string | `"2026-06-15"` | ~5% | ISO date of planned deprecation |
| `metadata` | object | `{"comments": "..."}` | ~5% | Freeform metadata |
| `source` | string | URL | ~20% | Documentation link |
| `comment` | string | prose | ~5% | Human note |
| `air_travel_cost_per_mile` | float | varies | <1% | Non-AI cost (ignore) |

### Representative Sample — 5 Models

```json
// gpt-4o (OpenAI)
{
  "litellm_provider": "openai",
  "mode": "chat",
  "max_tokens": 16384,
  "max_input_tokens": 128000,
  "max_output_tokens": 16384,
  "input_cost_per_token": 2.5e-06,
  "output_cost_per_token": 1e-05,
  "input_cost_per_token_batches": 1.25e-06,
  "output_cost_per_token_batches": 5e-06,
  "cache_read_input_token_cost": 1.25e-06,
  "supports_function_calling": true,
  "supports_parallel_function_calling": true,
  "supports_vision": true,
  "supports_response_schema": true,
  "supports_native_structured_output": true,
  "supports_system_messages": true,
  "supports_tool_choice": true,
  "supports_prompt_caching": true,
  "supports_native_streaming": true,
  "supported_endpoints": ["/chat/completions", "/assistants", "/batch"],
  "supported_modalities": ["text", "image"],
  "supported_output_modalities": ["text"]
}

// claude-3-5-sonnet-20241022 (Anthropic)
{
  "litellm_provider": "anthropic",
  "mode": "chat",
  "max_tokens": 8192,
  "max_input_tokens": 200000,
  "max_output_tokens": 8192,
  "input_cost_per_token": 3e-06,
  "output_cost_per_token": 1.5e-05,
  "input_cost_per_token_batches": 1.5e-06,
  "output_cost_per_token_batches": 7.5e-06,
  "cache_creation_input_token_cost": 3.75e-06,
  "cache_creation_input_token_cost_above_1hr": 7.5e-06,
  "cache_read_input_token_cost": 3e-07,
  "supports_function_calling": true,
  "supports_parallel_function_calling": true,
  "supports_vision": true,
  "supports_pdf_input": true,
  "supports_prompt_caching": true,
  "supports_response_schema": true,
  "supports_tool_choice": true,
  "supports_assistant_prefill": true,
  "supports_computer_use": true,
  "supports_native_streaming": true,
  "supported_endpoints": ["/messages", "/messages/batches", "/messages/count_tokens"]
}

// o1 (OpenAI reasoning)
{
  "litellm_provider": "openai",
  "mode": "chat",
  "max_tokens": 100000,
  "max_input_tokens": 200000,
  "max_output_tokens": 100000,
  "input_cost_per_token": 1.5e-05,
  "output_cost_per_token": 6e-05,
  "output_cost_per_reasoning_token": 6e-05,
  "input_cost_per_token_batches": 7.5e-06,
  "output_cost_per_token_batches": 3e-05,
  "cache_read_input_token_cost": 7.5e-06,
  "supports_function_calling": true,
  "supports_vision": true,
  "supports_reasoning": true,
  "supports_max_reasoning_effort": true,
  "supports_minimal_reasoning_effort": true,
  "supports_none_reasoning_effort": true,
  "supports_response_schema": true,
  "supports_system_messages": true,
  "supports_native_streaming": true,
  "tool_use_system_prompt_tokens": 964
}

// gemini/gemini-1.5-pro (Google)
{
  "litellm_provider": "gemini",
  "mode": "chat",
  "max_tokens": 8192,
  "max_input_tokens": 2000000,
  "max_output_tokens": 8192,
  "input_cost_per_token": 1.25e-06,
  "output_cost_per_token": 5e-06,
  "input_cost_per_token_above_200k_tokens": 2.5e-06,
  "output_cost_per_token_above_200k_tokens": 1e-05,
  "input_cost_per_image": 0.001315,
  "supports_function_calling": true,
  "supports_vision": true,
  "supports_video_input": true,
  "supports_audio_input": true,
  "supports_pdf_input": true,
  "supports_response_schema": true,
  "supports_system_messages": true,
  "supports_tool_choice": true,
  "supports_prompt_caching": true,
  "supported_modalities": ["text", "image", "audio", "video"],
  "supported_output_modalities": ["text"]
}

// deepseek/deepseek-r1 (reasoning)
{
  "litellm_provider": "deepseek",
  "mode": "chat",
  "max_tokens": 8000,
  "max_input_tokens": 64000,
  "max_output_tokens": 8000,
  "input_cost_per_token": 5.5e-07,
  "output_cost_per_token": 2.19e-06,
  "output_cost_per_reasoning_token": 2.19e-06,
  "cache_read_input_token_cost": 1.4e-07,
  "supports_function_calling": true,
  "supports_reasoning": true,
  "supports_native_streaming": true
}
```

### Capability Flags (the `supports_*` Family)

| Flag | Enables |
|------|---------|
| `supports_function_calling` | Tool use / function calling; required before sending `tools[]` |
| `supports_parallel_function_calling` | Calling multiple tools in one response |
| `supports_tool_choice` | The `tool_choice` field (auto/any/none/forced) |
| `supports_vision` | Image input in messages |
| `supports_image_input` | Alias for vision |
| `supports_audio_input` | Audio/speech transcription input |
| `supports_audio_output` | TTS / voice output |
| `supports_video_input` | Video frame analysis |
| `supports_pdf_input` | PDF/document content blocks |
| `supports_embedding_image_input` | Image input for embeddings |
| `supports_prompt_caching` | `cache_control` on content blocks |
| `supports_response_schema` | `response_format: {type: "json_schema"}` |
| `supports_native_structured_output` | Provider-level structured output guarantee |
| `supports_system_messages` | `system` field or `role: "system"` |
| `supports_assistant_prefill` | `role: "assistant"` as last message (Anthropic) |
| `supports_reasoning` | Extended thinking / chain-of-thought output |
| `supports_max_reasoning_effort` | `reasoning_effort: high` |
| `supports_minimal_reasoning_effort` | `reasoning_effort: low` |
| `supports_none_reasoning_effort` | `reasoning_effort: none` (disable) |
| `supports_xhigh_reasoning_effort` | `reasoning_effort: xhigh` (Anthropic) |
| `supports_web_search` | Built-in web search tool |
| `supports_computer_use` | `computer_use` tool type |
| `supports_native_streaming` | Native SSE (not simulated) |
| `supports_max_reasoning_effort` | Reasoning effort level control |

### Their Config Shape (Operator-Facing)

LiteLLM splits operator config into `litellm_params` (routing/execution) and `model_info` (metadata/ACL):

```yaml
model_list:
  - model_name: claude-3-5-sonnet        # user-visible name
    litellm_params:
      model: anthropic/claude-3-5-sonnet-20241022  # wire name
      api_key: os.environ/ANTHROPIC_API_KEY
      api_base: https://api.anthropic.com  # optional override
      max_tokens: 4096
      rpm: 500
      tpm: 100000
    model_info:
      version: 1
      supported_environments: ["production", "staging"]
      access_groups: ["team-a"]
      description: "Claude 3.5 Sonnet for production workloads"

  - model_name: gpt-4o-team1
    litellm_params:
      model: azure/gpt-4o
      api_base: https://mydeployment.openai.azure.com/
      api_version: "2024-08-01-preview"
      litellm_credential_name: azure_prod_cred
    model_info:
      supported_environments: ["production"]
```

The `litellm_params.model` field is a compound string: `provider/model-id`. The `model_info` block is thin — no pricing, no capabilities. Capabilities come from the master JSON file by model name lookup.

---

## 2. OpenRouter — What They Capture

**Source:** `https://openrouter.ai/api/v1/models` (live, ~100+ models)
**Provider metadata source:** `https://openrouter.ai/api/v1/providers` (exists, 90 providers)

### Model Endpoint Fields

| Field | Type | Example | Frequency | Description |
|-------|------|---------|-----------|-------------|
| `id` | string | `"anthropic/claude-opus-4.7"` | 100% | `provider/model-slug` composite ID |
| `canonical_slug` | string | `"anthropic/claude-4.7-opus-20260416"` | 100% | Versioned canonical ID |
| `hugging_face_id` | string\|null | `"meta-llama/Meta-Llama-3.1-70B-Instruct"` | ~60% | HuggingFace model card link |
| `name` | string | `"Anthropic: Claude Opus 4.7"` | 100% | Display name |
| `created` | int | `1776351100` | 100% | Unix timestamp of listing |
| `description` | string | `"Opus 4.7 is..."` | ~95% | Prose description (1–4 sentences) |
| `context_length` | int | `1000000` | 100% | Total context window |
| `architecture` | object | see below | 100% | Modality + tokenizer info |
| `pricing` | object | see below | 100% | Cost per token/unit |
| `top_provider` | object | see below | 100% | Provider-level limits |
| `per_request_limits` | null | `null` | ~100% null | Reserved, always null currently |
| `supported_parameters` | array[string] | `["tools","max_tokens",...]` | ~90% | OpenAI param names accepted |
| `default_parameters` | object | `{"temperature": 0.6, "top_p": 0.95}` | ~20% | Defaults applied if not supplied |
| `supported_voices` | null | `null` | ~100% null | Reserved for TTS models |
| `knowledge_cutoff` | string\|null | `"2025-09-01"` | ~30% | Knowledge cutoff date string |
| `expiration_date` | string\|null | `null` | ~5% | ISO date when model is removed |
| `links` | object | `{"details": "/api/v1/models/..."}` | 100% | Self-link to detail endpoint |

**`architecture` subobject:**
| Field | Type | Example |
|-------|------|---------|
| `modality` | string | `"text+image->text"` (legacy compound string) |
| `input_modalities` | array[string] | `["text","image"]` |
| `output_modalities` | array[string] | `["text"]` |
| `tokenizer` | string | `"Claude"`, `"GPT"`, `"Gemini"`, `"Mistral"`, `"DeepSeek"`, `"Router"`, `"Other"` |
| `instruct_type` | string\|null | `null` (mostly null for instruct models now) |

**`top_provider` subobject:**
| Field | Type | Notes |
|-------|------|-------|
| `context_length` | int | Provider's actual limit (may differ from model's `context_length`) |
| `max_completion_tokens` | int\|null | Max output tokens; null if no explicit cap |
| `is_moderated` | bool | Whether provider applies content moderation |

**`links` subobject:**
| Field | Type | Notes |
|-------|------|-------|
| `details` | string | API path for the model detail endpoint |

### Representative Sample — 5 Models

```json
// anthropic/claude-opus-4.7
{
  "id": "anthropic/claude-opus-4.7",
  "canonical_slug": "anthropic/claude-4.7-opus-20260416",
  "name": "Anthropic: Claude Opus 4.7",
  "created": 1776351100,
  "description": "Opus 4.7 is the next generation of Anthropic's Opus family, built for long-running, asynchronous agents.",
  "context_length": 1000000,
  "architecture": {
    "modality": "text+image->text",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "tokenizer": "Claude",
    "instruct_type": null
  },
  "pricing": {
    "prompt": "0.000005",
    "completion": "0.000025",
    "web_search": "0.01",
    "input_cache_read": "0.0000005",
    "input_cache_write": "0.00000625"
  },
  "top_provider": {
    "context_length": 1000000,
    "max_completion_tokens": 128000,
    "is_moderated": false
  },
  "supported_parameters": ["include_reasoning","max_tokens","reasoning","response_format","stop","structured_outputs","tool_choice","tools","verbosity"],
  "knowledge_cutoff": null,
  "expiration_date": null
}

// google/gemini-3.1-pro-preview (multimodal pricing example)
{
  "id": "google/gemini-3.1-pro-preview",
  "name": "Google: Gemini 3.1 Pro Preview",
  "context_length": 1048576,
  "architecture": {
    "input_modalities": ["audio","file","image","text","video"],
    "output_modalities": ["text"],
    "tokenizer": "Gemini"
  },
  "pricing": {
    "prompt": "0.000002",
    "completion": "0.000012",
    "image": "0.000002",
    "audio": "0.000002",
    "web_search": "0.014",
    "internal_reasoning": "0.000012",
    "input_cache_read": "0.0000002",
    "input_cache_write": "0.000000375"
  },
  "top_provider": {"context_length": 1048576, "max_completion_tokens": 65536, "is_moderated": false},
  "knowledge_cutoff": null
}

// deepseek/deepseek-v4-pro
{
  "id": "deepseek/deepseek-v4-pro",
  "context_length": 1048576,
  "architecture": {"input_modalities": ["text"], "output_modalities": ["text"], "tokenizer": "DeepSeek"},
  "pricing": {
    "prompt": "0.000000435",
    "completion": "0.00000087",
    "input_cache_read": "0.000000003625"
  },
  "top_provider": {"context_length": 1048576, "max_completion_tokens": 384000, "is_moderated": false}
}

// x-ai/grok-4.20
{
  "id": "x-ai/grok-4.20",
  "context_length": 2000000,
  "architecture": {"input_modalities": ["text","image","file"], "output_modalities": ["text"], "tokenizer": "Grok"},
  "pricing": {"prompt": "0.00000125", "completion": "0.0000025", "web_search": "0.005", "input_cache_read": "0.0000002"},
  "knowledge_cutoff": "2025-09-01"
}

// nvidia/nemotron-3-nano-omni:free (zero-cost model)
{
  "id": "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
  "context_length": 256000,
  "architecture": {"input_modalities": ["text","audio","image","video"], "output_modalities": ["text"]},
  "pricing": {"prompt": "0", "completion": "0"},
  "default_parameters": {"temperature": 0.6, "top_p": 0.95}
}
```

### Pricing Structure

All pricing fields are **string-encoded decimal USD per token** (not per million). Caller multiplies by 1e6 to get per-million USD.

| Field | Unit | Notes |
|-------|------|-------|
| `prompt` | USD/token | Input tokens; `"0"` = free; `"-1"` = router/variable |
| `completion` | USD/token | Output tokens |
| `input_cache_read` | USD/token | Cached input read (cheaper than `prompt`) |
| `input_cache_write` | USD/token | Cache write (typically 1.25x `prompt` for Anthropic) |
| `web_search` | USD/request | Per search query (not per token) |
| `image` | USD/token or USD/image | Image input unit cost |
| `audio` | USD/token | Audio input unit cost |
| `internal_reasoning` | USD/token | Reasoning token surcharge (Gemini/Google) |

Special values:
- `"0"` — free tier
- `"-1"` — router model with variable/unknown pricing
- Missing field — not applicable for this model

OpenRouter **does not** express batching discounts or tiered pricing (e.g., above-200k tiers). Those details are lost in the abstraction.

### Architecture / Capabilities

OpenRouter does not expose boolean capability flags. Capabilities are inferred from:
1. `architecture.input_modalities` — tells you what input types are accepted
2. `architecture.output_modalities` — tells you output types
3. `supported_parameters` — the OpenAI parameter names the model accepts; presence of `"tools"` implies tool use, `"reasoning"` implies thinking, `"structured_outputs"` implies schema output
4. `architecture.tokenizer` — identifies the model family for tokenization

There is no explicit `supports_prompt_caching`, `supports_computer_use`, etc. The `supported_parameters` list is the closest analog.

### Provider Metadata

**Endpoint:** `GET https://openrouter.ai/api/v1/providers` — **exists**, returns 90 provider objects.

```json
// Anthropic
{
  "name": "Anthropic",
  "slug": "anthropic",
  "privacy_policy_url": "https://www.anthropic.com/legal/privacy",
  "terms_of_service_url": "https://www.anthropic.com/legal/commercial-terms",
  "status_page_url": "https://status.anthropic.com/",
  "headquarters": "US",
  "datacenters": null
}

// OpenAI
{
  "name": "OpenAI",
  "slug": "openai",
  "privacy_policy_url": "https://openai.com/policies/privacy-policy/",
  "terms_of_service_url": "https://openai.com/policies/row-terms-of-use/",
  "status_page_url": "https://status.openai.com/",
  "headquarters": "US",
  "datacenters": null
}

// Moonshot AI (regional example)
{
  "name": "Moonshot AI",
  "slug": "moonshotai",
  "privacy_policy_url": "https://platform.moonshot.ai/docs/agreement/userprivacy",
  "terms_of_service_url": "https://platform.moonshot.ai/docs/agreement/modeluse",
  "status_page_url": null,
  "headquarters": "SG",
  "datacenters": ["SG"]
}
```

Provider objects do NOT include: logo URL, homepage URL, description, or console/billing page. Those must be supplied by Wyolet separately.

---

## 3. Side-by-Side Comparison

### Pricing

| Concept | LiteLLM | OpenRouter | Wyolet (current) | Wyolet (proposed) |
|---------|---------|------------|-----------------|-------------------|
| Input tokens | `input_cost_per_token` (USD/token) | `pricing.prompt` (string USD/token) | `Pricing.Input` (USD/million) | `Pricing.Rates["input"]` (USD/million) |
| Output tokens | `output_cost_per_token` | `pricing.completion` | `Pricing.Output` (USD/million) | `Pricing.Rates["output"]` (USD/million) |
| Cached input read | `cache_read_input_token_cost` | `pricing.input_cache_read` | `Pricing.CachedInput` | `Pricing.Rates["cache_read"]` |
| Cached input write | `cache_creation_input_token_cost` | `pricing.input_cache_write` | — (missing) | `Pricing.Rates["cache_write"]` |
| Batch input | `input_cost_per_token_batches` | — (not exposed) | — (missing) | `Pricing.Rates["batch_input"]` |
| Batch output | `output_cost_per_token_batches` | — (not exposed) | — (missing) | `Pricing.Rates["batch_output"]` |
| Reasoning tokens | `output_cost_per_reasoning_token` | `pricing.internal_reasoning` | — (missing) | `Pricing.Rates["reasoning"]` |
| Web search | `search_context_cost_per_query` (tiered object) | `pricing.web_search` (per query) | — | `Pricing.Rates["web_search"]` |
| Image input | `input_cost_per_image` | `pricing.image` | — | `Pricing.Rates["image_input"]` |
| Audio input | `input_cost_per_audio_token` | `pricing.audio` | — | `Pricing.Rates["audio_input"]` |
| Tiered pricing (200k+) | Separate `*_above_200k_tokens` fields | — (not exposed) | — | Future: `Pricing.Tiers []PricingTier` (defer) |
| Pricing unit | per-token float | per-token string | per-million float | per-million float (keep) |
| Currency | implicit USD | implicit USD | implicit USD | `Pricing.Currency string` (default "USD") |

### Capabilities

| Capability | LiteLLM flag | OpenRouter | Wyolet (current) | Wyolet (proposed) |
|-----------|-------------|------------|-----------------|-------------------|
| Chat / completion | `mode: "chat"` | `output_modalities` includes `"text"` | `Capabilities.Chat` | `Capabilities.Chat` (keep) |
| Streaming | `supports_native_streaming` | `supported_parameters` has none | `Capabilities.Streaming` | `Capabilities.Streaming` (keep) |
| Tools / function calling | `supports_function_calling` | `supported_parameters` has `"tools"` | `Capabilities.Tools` | `Capabilities.Tools` (keep) |
| Parallel tools | `supports_parallel_function_calling` | `supported_parameters` implicit | — | `Capabilities.ParallelTools` (add) |
| Vision / image input | `supports_vision` / `supports_image_input` | `input_modalities` has `"image"` | `Capabilities.Vision` | `Capabilities.Vision` (keep) |
| Audio input | `supports_audio_input` | `input_modalities` has `"audio"` | `Capabilities.Audio` (merged) | `Capabilities.AudioInput` (split) |
| Audio output | `supports_audio_output` | `output_modalities` has `"audio"` | `Capabilities.Audio` (merged) | `Capabilities.AudioOutput` (split) |
| Video input | `supports_video_input` | `input_modalities` has `"video"` | — | `Capabilities.VideoInput` (add) |
| PDF / document input | `supports_pdf_input` | `input_modalities` has `"file"` | — | `Capabilities.FileInput` (add; covers PDF + file) |
| Prompt caching | `supports_prompt_caching` | `pricing.input_cache_read` non-zero | — | `Capabilities.PromptCache` (add) |
| JSON mode | `supports_response_schema` (partially) | `supported_parameters` has `"response_format"` | `Capabilities.JSONMode` | `Capabilities.JSONMode` (keep) |
| Structured output (schema) | `supports_native_structured_output` | `supported_parameters` has `"structured_outputs"` | `Capabilities.StructuredOutput` | `Capabilities.StructuredOutput` (keep) |
| Reasoning / thinking | `supports_reasoning` | `supported_parameters` has `"reasoning"` | `Capabilities.Reasoning` | `Capabilities.Reasoning` (keep) |
| System messages | `supports_system_messages` | implicit | — | `Capabilities.SystemMessages` (add) |
| Assistant prefill | `supports_assistant_prefill` | — | — | `Capabilities.AssistantPrefill` (add; Anthropic-specific) |
| Computer use | `supports_computer_use` | — | — | `Capabilities.ComputerUse` (add) |
| Web search | `supports_web_search` | `pricing.web_search` non-zero | — | `Capabilities.WebSearch` (add) |
| Batch API | `supported_endpoints` has `/batches` | — | — | `Capabilities.Batch` (add) |
| Embeddings | `mode: "embedding"` | — | `Capabilities.Embeddings` | `Capabilities.Embeddings` (keep; deferred) |

### Context Windows

| Concept | LiteLLM | OpenRouter | Wyolet (current) | Wyolet (proposed) |
|---------|---------|------------|-----------------|-------------------|
| Input window | `max_input_tokens` | `context_length` | `ContextWindow` (single int) | `ContextWindow.InputMax int` |
| Output cap | `max_output_tokens` | `top_provider.max_completion_tokens` | `MaxOutputTokens int` | `ContextWindow.OutputMax int` |
| Total context | — | `context_length` (used as total) | `ContextWindow` | `ContextWindow.Total int` (= InputMax for most models) |
| Legacy alias | `max_tokens` (= `max_output_tokens`) | — | — | deprecated; migrate to `OutputMax` |

### Display Metadata

| Field | LiteLLM | OpenRouter | Wyolet (proposed) |
|-------|---------|------------|-------------------|
| Display name | — (none) | `name` | `DisplayName string` |
| Description | `metadata.comments` (rare) | `description` | `Description string` |
| Family | — | `architecture.tokenizer` (approximate) | `Family string` (keep, existing) |
| Release date | — | `created` (unix timestamp of listing) | `ReleaseDate string` (ISO date) |
| Knowledge cutoff | — | `knowledge_cutoff` (string or null) | `KnowledgeCutoff string` |
| Deprecation date | `deprecation_date` (ISO string) | `expiration_date` (ISO string or null) | `Deprecation.SunsetDate string` |
| Deprecation status | implied by `deprecation_date` | implied by `expiration_date` | `Deprecation.Status string` (`"active"`, `"deprecated"`, `"retired"`) |
| Replacement model | — | — | `Deprecation.Replacement string` |
| License | — | — | `License string` (keep, existing) |
| Tags | — | — | `Tags []string` |
| Logo URL | — | — (no model logo) | provider-level `LogoURL string` |
| Provider page URL | `source` (link to docs) | `links.details` (self-link only) | `ProviderModelPageURL string` |
| HuggingFace ID | — | `hugging_face_id` | consider adding; deferred |

---

## 4. Proposed Wyolet Schema

### `catalog.Provider.Spec` — Display + Behavior

```go
type ProviderSpec struct {
    // Behavior (existing — keep as-is)
    Kind        ProviderKind `yaml:"kind"        json:"kind"`
    BaseURL     string       `yaml:"baseURL"     json:"baseURL"`
    Default     bool         `yaml:"default,omitempty"     json:"default,omitempty"`
    DefaultPool string       `yaml:"defaultPool,omitempty" json:"defaultPool,omitempty"`

    // Display (NEW)
    DisplayName   string `yaml:"displayName,omitempty"   json:"displayName,omitempty"`
    Description   string `yaml:"description,omitempty"   json:"description,omitempty"`
    HomepageURL   string `yaml:"homepageURL,omitempty"   json:"homepageURL,omitempty"`
    DocsURL       string `yaml:"docsURL,omitempty"       json:"docsURL,omitempty"`
    ConsoleURL    string `yaml:"consoleURL,omitempty"    json:"consoleURL,omitempty"`
    StatusPageURL string `yaml:"statusPageURL,omitempty" json:"statusPageURL,omitempty"`
    LogoURL       string `yaml:"logoURL,omitempty"       json:"logoURL,omitempty"`
}
```

Field justifications:
- `DisplayName` — OpenRouter provider objects have `name`; needed for admin UI provider pages. Without it the UI must title-case the kind string.
- `Description` — OpenRouter has 1–4 sentence model descriptions; provider-level equivalent for provider pages.
- `HomepageURL` — OpenRouter providers lack this; needed for admin UI links. Example: `https://www.anthropic.com`.
- `DocsURL` — OpenRouter lacks this; needed for operator self-help. Example: `https://docs.anthropic.com`.
- `ConsoleURL` — Where the customer manages billing/API keys for this provider. Not in either source; derived from common sense. Example: `https://console.anthropic.com`.
- `StatusPageURL` — OpenRouter providers have `status_page_url`; directly adopted. Enables monitoring links in admin UI.
- `LogoURL` — Neither LiteLLM nor OpenRouter expose provider logos in their API; Wyolet needs it for the admin UI. Recommend hosting static assets at `https://wyolet.dev/logos/<name>.svg`.

**Note on `DefaultPricing`:** Not proposed. Provider-level pricing inheritance adds complexity for a marginal use case (most providers have per-model pricing). Models that share the same pricing can duplicate the field; YAML is cheap. Revisit if a provider with 50+ models at identical pricing is added.

### `catalog.Model.Spec` — Model Card

```go
type ModelSpec struct {
    // Identity (existing — keep)
    Provider     string `yaml:"provider"     json:"provider"`
    UpstreamName string `yaml:"upstreamName" json:"upstreamName"`
    Family       string `yaml:"family,omitempty" json:"family,omitempty"`
    Version      string `yaml:"version,omitempty" json:"version,omitempty"`
    License      string `yaml:"license,omitempty" json:"license,omitempty"`

    // Display (partially existing, expand)
    DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"`
    Description string `yaml:"description,omitempty" json:"description,omitempty"`
    // Description existed; DisplayName is new — allows short "GPT-4o" vs description prose.

    // Release / lifecycle (existing fields stay, new Deprecation struct)
    ReleaseDate     string      `yaml:"releaseDate,omitempty"     json:"releaseDate,omitempty"`
    KnowledgeCutoff string      `yaml:"knowledgeCutoff,omitempty" json:"knowledgeCutoff,omitempty"`
    Deprecation     *Deprecation `yaml:"deprecation,omitempty"    json:"deprecation,omitempty"`
    // DeprecationDate (current plain string) is absorbed into Deprecation.SunsetDate.
    // Old field is kept for parsing compat; loader promotes it into Deprecation struct.

    // Context (split existing single int into struct)
    ContextWindow ContextWindow `yaml:"contextWindow,omitempty" json:"contextWindow,omitempty"`
    // Replaces old: ContextWindow int + MaxOutputTokens int.
    // Old flat fields accepted on input for backward compat; struct is the canonical shape.

    // Capabilities (expand existing struct)
    Capabilities Capabilities `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
    Modalities   Modalities   `yaml:"modalities,omitempty"   json:"modalities,omitempty"`

    // Pricing (replace existing flat struct)
    Pricing *Pricing `yaml:"pricing,omitempty" json:"pricing,omitempty"`

    // Links
    ProviderModelPageURL string `yaml:"providerModelPageURL,omitempty" json:"providerModelPageURL,omitempty"`
    Documentation        string `yaml:"documentation,omitempty"        json:"documentation,omitempty"`
    // Documentation existed; ProviderModelPageURL is new (e.g. https://docs.anthropic.com/en/docs/about-claude/models/claude-opus-4-7)

    // Filtering
    Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`

    // Rate limits (existing — keep)
    RateLimits []RateLimitAttachment `yaml:"rateLimits,omitempty" json:"rateLimits,omitempty"`
}

type ContextWindow struct {
    // InputMax is the max context length in tokens (prompt + history).
    // Corresponds to LiteLLM's max_input_tokens, OpenRouter's context_length.
    InputMax int `yaml:"inputMax,omitempty" json:"inputMax,omitempty"`

    // OutputMax is the max tokens the model will generate.
    // Corresponds to LiteLLM's max_output_tokens, OpenRouter's top_provider.max_completion_tokens.
    OutputMax int `yaml:"outputMax,omitempty" json:"outputMax,omitempty"`

    // Total is the total window size when input+output share a single pool.
    // Many providers set Total == InputMax. Use InputMax when unsure.
    Total int `yaml:"total,omitempty" json:"total,omitempty"`
}

type Deprecation struct {
    // Status is "active", "deprecated", or "retired".
    Status string `yaml:"status,omitempty" json:"status,omitempty"`

    // SunsetDate is the ISO date after which the model will stop accepting requests.
    // Corresponds to LiteLLM's deprecation_date, OpenRouter's expiration_date.
    SunsetDate string `yaml:"sunsetDate,omitempty" json:"sunsetDate,omitempty"`

    // Replacement is the model name (catalog name, not upstream name) to migrate to.
    Replacement string `yaml:"replacement,omitempty" json:"replacement,omitempty"`
}

type Pricing struct {
    // Currency is always "USD" for now. Explicit so clients don't assume.
    Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`

    // Unit is the denomination for Rates values.
    // "per_million" means USD per million tokens (Wyolet convention).
    // Use "per_request" for flat-rate meters (web_search, image).
    Unit string `yaml:"unit,omitempty" json:"unit,omitempty"`

    // Rates maps meter name to USD cost in Unit denomination.
    // Standard keys: "input", "output", "cache_read", "cache_write",
    //                "batch_input", "batch_output", "reasoning",
    //                "web_search", "image_input", "audio_input".
    Rates map[string]float64 `yaml:"rates,omitempty" json:"rates,omitempty"`

    // Legacy flat fields — accepted on input, ignored on output if Rates is set.
    // Input is USD/million input tokens.
    Input       float64 `yaml:"input,omitempty"       json:"input,omitempty"`
    CachedInput float64 `yaml:"cachedInput,omitempty" json:"cachedInput,omitempty"`
    Output      float64 `yaml:"output,omitempty"      json:"output,omitempty"`
}
```

### Capability Flags — Final List

**Recommendation: closed struct with boolean fields (not `map[string]bool`).**

Rationale: routing and rate-limit logic (e.g., "can this model handle the tools the user sent?") needs to check capability at sub-microsecond speed on the hot path. A map lookup with string key allocation is measurably slower than a struct field access and prevents the compiler from producing dead-code warnings on unused capabilities. The closed set also makes schema validation trivial (unknown YAML keys = error, not silent ignore). When a genuinely new capability arises (e.g., `ComputerUse` was unknown 18 months ago), adding a field to a struct is a one-line change. The `map[string]bool` approach solves a flexibility problem we don't actually have — providers don't invent capability names monthly.

```go
type Capabilities struct {
    // Core (existing — keep)
    Chat      bool `yaml:"chat,omitempty"      json:"chat,omitempty"`
    Streaming bool `yaml:"streaming,omitempty" json:"streaming,omitempty"`

    // Tool use
    Tools         bool `yaml:"tools,omitempty"         json:"tools,omitempty"`
    ParallelTools  bool `yaml:"parallelTools,omitempty"  json:"parallelTools,omitempty"`

    // Input modalities
    Vision     bool `yaml:"vision,omitempty"     json:"vision,omitempty"`
    AudioInput  bool `yaml:"audioInput,omitempty"  json:"audioInput,omitempty"`
    AudioOutput bool `yaml:"audioOutput,omitempty" json:"audioOutput,omitempty"`
    VideoInput  bool `yaml:"videoInput,omitempty"  json:"videoInput,omitempty"`
    FileInput   bool `yaml:"fileInput,omitempty"   json:"fileInput,omitempty"` // PDF + document upload

    // Caching
    PromptCache bool `yaml:"promptCache,omitempty" json:"promptCache,omitempty"`

    // Output format
    JSONMode         bool `yaml:"jsonMode,omitempty"         json:"jsonMode,omitempty"`
    StructuredOutput bool `yaml:"structuredOutput,omitempty" json:"structuredOutput,omitempty"`

    // Reasoning
    Reasoning bool `yaml:"reasoning,omitempty" json:"reasoning,omitempty"`

    // Message roles
    SystemMessages   bool `yaml:"systemMessages,omitempty"   json:"systemMessages,omitempty"`
    AssistantPrefill bool `yaml:"assistantPrefill,omitempty" json:"assistantPrefill,omitempty"`

    // Special capabilities
    ComputerUse bool `yaml:"computerUse,omitempty" json:"computerUse,omitempty"`
    WebSearch   bool `yaml:"webSearch,omitempty"   json:"webSearch,omitempty"`
    Batch       bool `yaml:"batch,omitempty"       json:"batch,omitempty"`

    // Deferred (keep existing, not actively used in routing yet)
    Embeddings bool `yaml:"embeddings,omitempty" json:"embeddings,omitempty"`

    // REMOVED: Audio bool — was a merged audio-in/out flag; split into AudioInput + AudioOutput.
    // Old YAML with `audio: true` should be migrated to audioInput: true / audioOutput: true.
}
```

Per-flag routing/billing relevance:

| Flag | Routing use | Rate-limit/billing use |
|------|------------|----------------------|
| `Tools` | Gate tool-bearing requests; return 400 if false | No |
| `Vision` | Gate image content blocks | No |
| `FileInput` | Gate document content blocks | No |
| `PromptCache` | Decide whether to forward cache_control headers | `cache_read` / `cache_write` rates apply |
| `Reasoning` | Forward thinking params; parse thinking blocks | `reasoning` rate may apply |
| `Batch` | Enable batch submission path | `batch_input` / `batch_output` rates apply |
| `StructuredOutput` | Gate response_format=json_schema | No |
| `SystemMessages` | Decide whether to hoist system role | No |
| `AssistantPrefill` | Decide whether to allow assistant-role last message | No |
| `WebSearch` | Forward search tool params | `web_search` rate applies |
| `ComputerUse` | Forward beta header | `computer_use_input` / `output` rates apply |

---

## 5. Three Worked Examples

### Example 1: Anthropic / claude-sonnet-4-7

`config/providers/anthropic/provider.yaml`
```yaml
apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: anthropic
spec:
  kind: anthropic
  baseURL: https://api.anthropic.com
  defaultPool: anthropic-default
  displayName: Anthropic
  description: "AI safety company building reliable, interpretable, and steerable AI systems."
  homepageURL: https://www.anthropic.com
  docsURL: https://docs.anthropic.com
  consoleURL: https://console.anthropic.com
  statusPageURL: https://status.anthropic.com
  logoURL: https://wyolet.dev/logos/anthropic.svg
```

`config/providers/anthropic/models/claude-sonnet-4-7.yaml`
```yaml
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: claude-sonnet-4-7
  labels:
    family: claude-4
    runtime: anthropic
    tier: balanced
spec:
  provider: anthropic
  upstreamName: claude-sonnet-4-7
  displayName: "Claude Sonnet 4.7"
  description: "Balanced speed and intelligence for production workloads. Successor to Claude 3.5 Sonnet."
  family: claude-4
  releaseDate: "2026-04-16"
  knowledgeCutoff: "2025-04"
  contextWindow:
    inputMax: 1000000
    outputMax: 64000
    total: 1000000
  capabilities:
    chat: true
    streaming: true
    tools: true
    parallelTools: true
    vision: true
    fileInput: true
    promptCache: true
    jsonMode: true
    structuredOutput: true
    reasoning: true
    systemMessages: true
    batch: true
    webSearch: true
  modalities:
    input: [text, image]
    output: [text]
  pricing:
    currency: USD
    unit: per_million
    rates:
      input: 3.00
      output: 15.00
      cache_write: 3.75
      cache_read: 0.30
      batch_input: 1.50
      batch_output: 7.50
  providerModelPageURL: https://docs.anthropic.com/en/docs/about-claude/models/claude-sonnet-4-7
  tags: ["balanced", "vision", "caching", "reasoning"]
```

### Example 2: OpenAI / gpt-4o

`config/providers/openai/provider.yaml` (existing, with new display fields added)
```yaml
apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: openai-prod
spec:
  kind: openai
  baseURL: https://api.openai.com
  defaultPool: openai-default
  displayName: OpenAI
  description: "Creator of the GPT model family and the de facto standard OpenAI API."
  homepageURL: https://openai.com
  docsURL: https://platform.openai.com/docs
  consoleURL: https://platform.openai.com
  statusPageURL: https://status.openai.com
  logoURL: https://wyolet.dev/logos/openai.svg
```

`config/providers/openai/models/gpt-4o.yaml`
```yaml
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: gpt-4o
  labels:
    family: gpt-4
    runtime: openai
    tier: flagship
spec:
  provider: openai-prod
  upstreamName: gpt-4o
  displayName: "GPT-4o"
  description: "OpenAI's flagship multimodal model. Accepts text and image input; fast, low-latency responses."
  family: gpt-4
  releaseDate: "2024-05-13"
  knowledgeCutoff: "2024-10"
  contextWindow:
    inputMax: 128000
    outputMax: 16384
    total: 128000
  capabilities:
    chat: true
    streaming: true
    tools: true
    parallelTools: true
    vision: true
    promptCache: true
    jsonMode: true
    structuredOutput: true
    systemMessages: true
    batch: true
  modalities:
    input: [text, image]
    output: [text]
  pricing:
    currency: USD
    unit: per_million
    rates:
      input: 2.50
      output: 10.00
      cache_read: 1.25
      batch_input: 1.25
      batch_output: 5.00
  providerModelPageURL: https://platform.openai.com/docs/models/gpt-4o
  tags: ["flagship", "vision", "fast"]
```

### Example 3: Ollama / gemma4

`config/providers/ollama/provider.yaml`
```yaml
apiVersion: relay.wyolet.dev/v1
kind: Provider
metadata:
  name: dev-ollama
spec:
  kind: ollama
  baseURL: http://localhost:11434
  default: true
  defaultPool: ollama-default
  displayName: Ollama (local)
  description: "Local model inference via Ollama. Self-hosted; no API keys required."
  # homepageURL, docsURL, consoleURL, statusPageURL, logoURL: omitted — not applicable for local runner
```

`config/providers/ollama/models/gemma4.yaml` (excerpt — RateLimit objects unchanged)
```yaml
apiVersion: relay.wyolet.dev/v1
kind: Model
metadata:
  name: gemma4
  labels:
    family: gemma
    runtime: ollama
spec:
  provider: dev-ollama
  upstreamName: gemma4:latest
  displayName: "Gemma 4 8B (local)"
  description: "Gemma 4 8B running locally via Ollama. Q4_K_M quantization."
  family: gemma4
  license: gemma-terms
  contextWindow:
    inputMax: 8192
    outputMax: 8192
    total: 8192
  capabilities:
    chat: true
    streaming: true
    # vision, tools, etc.: false (omitted)
  modalities:
    input: [text]
    output: [text]
  # pricing: omitted — self-hosted, no per-token billing
  # releaseDate, knowledgeCutoff, providerModelPageURL: omitted — operator-managed
  tags: ["local", "open-weights"]
  rateLimits:
    - ref: gemma4-rpm
    - ref: gemma4-tpm
    - ref: gemma4-concurrency
```

---

## 6. Migration Notes

### `Provider.Spec` New Fields

| Field | Required? | Validation | If Missing |
|-------|-----------|-----------|------------|
| `DisplayName` | Optional | Non-empty string if present | Admin UI falls back to `Metadata.Name` title-cased |
| `Description` | Optional | ≤500 chars recommended | Hidden in admin UI |
| `HomepageURL` | Optional | Valid URL if present | No link shown |
| `DocsURL` | Optional | Valid URL if present | No link shown |
| `ConsoleURL` | Optional | Valid URL if present | No link shown |
| `StatusPageURL` | Optional | Valid URL if present | No link shown |
| `LogoURL` | Optional | Valid URL if present | Default placeholder logo |

All new Provider fields are optional. Existing provider YAML (e.g., `config/providers/openai/provider.yaml`) must continue to parse without modification.

### `Model.Spec` New and Changed Fields

| Field | Required? | Validation | If Missing | Migration Action |
|-------|-----------|-----------|------------|-----------------|
| `DisplayName` | Optional | Non-empty string if present | Falls back to `Metadata.Name` | Add to new models; existing OK |
| `Deprecation.Status` | Optional | One of `"active"`, `"deprecated"`, `"retired"` | Treated as `"active"` | — |
| `Deprecation.SunsetDate` | Optional | ISO date string (`2006-01-02` format) | No sunset logic applied | Migrate from flat `DeprecationDate` field |
| `Deprecation.Replacement` | Optional | Must be a known catalog model name | Log warning if model not found | — |
| `ContextWindow.InputMax` | Optional | `> 0` | Hot-path context check skipped | Migrate from flat `ContextWindow int` |
| `ContextWindow.OutputMax` | Optional | `> 0` | No output cap enforced | Migrate from flat `MaxOutputTokens int` |
| `ContextWindow.Total` | Optional | `>= InputMax` if both set | Defaults to `InputMax` | — |
| `Capabilities.PromptCache` | Optional | bool | `false` (cache_control headers dropped) | Add for Anthropic/OpenAI models |
| `Capabilities.ParallelTools` | Optional | bool | `false` | — |
| `Capabilities.AudioInput` | Optional | bool | `false` | Replaces old `Capabilities.Audio` |
| `Capabilities.AudioOutput` | Optional | bool | `false` | Replaces old `Capabilities.Audio` |
| `Capabilities.VideoInput` | Optional | bool | `false` | — |
| `Capabilities.FileInput` | Optional | bool | `false` | — |
| `Capabilities.SystemMessages` | Optional | bool | `false` | — |
| `Capabilities.AssistantPrefill` | Optional | bool | `false` | — |
| `Capabilities.ComputerUse` | Optional | bool | `false` | — |
| `Capabilities.WebSearch` | Optional | bool | `false` | — |
| `Capabilities.Batch` | Optional | bool | `false` | — |
| `Pricing.Rates` | Optional map | Values must be `>= 0` | No billing enrichment | Existing flat `Input`/`Output`/`CachedInput` continue to work |
| `Pricing.Currency` | Optional | Default `"USD"` | Treated as USD | — |
| `Pricing.Unit` | Optional | Default `"per_million"` | Treated as per-million | — |
| `ProviderModelPageURL` | Optional | Valid URL if present | No link shown | — |
| `Tags` | Optional | `[]string`, no empty strings | No tags shown in UI | — |

**Backward compatibility rule:** Old flat fields (`ContextWindow int`, `MaxOutputTokens int`, `DeprecationDate string`, `Pricing.Input/Output/CachedInput`) are accepted on input. On load, the catalog loader promotes them into the new struct shapes so all downstream code sees the new format. New YAML should use the new shapes only.

---

## 7. Things We Will NOT Take from LiteLLM / OpenRouter

**LiteLLM's `litellm_provider` field** — LiteLLM uses a flat `provider/model` string as the compound routing key (e.g., `"anthropic"`, `"bedrock_converse"`). Wyolet has a first-class `Provider` entity with typed `ProviderKind`. The compound-string approach creates ambiguity between logical providers (Anthropic) and deployment variants (Anthropic via Bedrock). We keep `ProviderKind` and add Bedrock as a separate `ProviderKind` when needed.

**LiteLLM's `mode` field** — `chat`, `completion`, `embedding`, `image_generation`, `audio_transcription`, `audio_speech`, `rerank`. Wyolet v1 is chat-only. Adding `mode` now would require routing logic branches we are not building yet. When embeddings land, add a `Mode ModelMode` field and treat `chat` as the zero value. Defer.

**OpenRouter's pricing as raw USD-per-token strings** — OpenRouter stores `"0.000005"` (USD per individual token). This is read-friendly but loses magnitude awareness (easy to confuse with per-thousand). Wyolet's `per_million` convention is clearer for humans writing YAML: `input: 3.00` vs `input: 0.000003`. Keep Wyolet's convention.

**OpenRouter's `top_provider` indirection** — OpenRouter uses `top_provider.context_length` and `top_provider.max_completion_tokens` as a second context window layer alongside `context_length`. The split exists because OpenRouter aggregates multiple providers behind one model ID. Wyolet has a 1:1 Provider:Model relationship, so there is no indirection needed. Fold into `ContextWindow.InputMax` / `ContextWindow.OutputMax`.

**OpenRouter's `canonical_slug`** — OpenRouter has two IDs: the user-visible `id` (e.g., `anthropic/claude-opus-4.7`) and a versioned `canonical_slug` (`anthropic/claude-4.7-opus-20260416`). Wyolet uses `Metadata.Name` (operator-chosen, stable) and `UpstreamName` (the wire ID sent to the provider). That covers the same ground without the two-slot indirection.

**LiteLLM's tiered pricing fields** (`*_above_200k_tokens`) — LiteLLM models every pricing tier as a flat field. This leads to an explosion: `cache_creation_input_token_cost_above_1hr_above_200k_tokens`. The proposed `Rates map` defers tiered pricing — it is not present in v1. When Gemini 200k+ tiers matter, add `Tiers []PricingTier` alongside `Rates`. Do not pre-populate the explosion now.

**OpenRouter's `architecture.instruct_type`** — Nearly always `null` in current data. This was meaningful when there were many instruct fine-tune naming conventions (`alpaca`, `vicuna`, etc.). Modern frontier models don't need it. If we ever support custom inference servers with non-standard prompt formats, use a `PromptFormat` field instead.

**OpenRouter's `hugging_face_id`** — Useful for open-weight models served via Ollama/vLLM. Add as a deferred optional field (`HuggingFaceID string`) only when we build a model auto-discovery flow from HuggingFace.

---

## 8. Open Questions

**Q1: Provider instance vs. provider type for display metadata.**
`DisplayName: "Anthropic"` makes sense for the Anthropic API directly. But if a future operator adds Anthropic via AWS Bedrock, they get `kind: bedrock` with a different `BaseURL`, effectively a different provider instance that represents the same underlying model vendor. Where does the Anthropic brand metadata live? Options: (a) `ProviderKind` has a static display registry in the Wyolet codebase (not in YAML), (b) `ProviderSpec.DisplayName` is per-instance and operators set it appropriately for each config, (c) introduce a `Brand string` field that is orthogonal to `Kind`. Option (b) is simplest and consistent with "operators own their config." Decision needed before PR.

**Q2: Model aliases (e.g., `claude-3-5-sonnet` → `claude-3-5-sonnet-20240620`).**
LiteLLM handles aliases by having both the alias and the versioned ID as top-level keys in the JSON, with the alias entry containing the same field values. OpenRouter uses `canonical_slug` for the pinned version and `id` for the friendly alias. Wyolet currently has no alias mechanism — every model in the catalog is an explicit entry with its own `Metadata.Name`. Should we add an `Aliases []string` to `ModelSpec` so an operator can register `claude-3-5-sonnet` and have it resolve to the versioned entry? Or require operators to create two catalog entries? Or add a separate `ModelAlias` kind? The choice affects client-facing model ID stability. Decision needed before PR.

**Q3: Closed `Capabilities` struct vs. open `map[string]bool`.**
Section 4 proposes a closed struct and justifies it. Restate the trade-off: a closed struct is fast, type-safe, and makes routing logic readable (`if spec.Capabilities.Tools`), but every new capability requires a code change + deploy. An open map supports operator-defined capability hints (e.g., a custom model with a custom flag) without a code change. The practical counter-argument: operator-defined capabilities have no effect unless routing code checks for them — so the map only buys you undocumented flags that do nothing. Unless there is a concrete use case for dynamic capability flags (e.g., a plugin routing system), the closed struct is the right call. Confirm this is acceptable before PR.

---

*Document generated 2026-05-07. Re-fetch sources before implementing if more than 30 days have elapsed.*
