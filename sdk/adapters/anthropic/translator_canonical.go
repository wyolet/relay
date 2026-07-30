// Package anthropic — AnthropicTranslator implements v1.Translator for the
// Anthropic Messages wire shape. Converts between Anthropic /v1/messages
// bodies and the canonical v1.Request/v1.Response types.
//
// Design decisions and known lossy mappings:
//   - cache_control: outbound (SerializeRequest) emits breakpoints from the
//     neutral v1.Request.CacheConfig (instructions/tools) and per-message
//     ItemCacheConfig anchors. Inbound (ParseRequest) still drops any wire
//     cache_control — we don't reverse-map vendor cache markers into canonical;
//     callers express cache intent via cache_config, not vendor fields.
//   - server_tool_use blocks (web_search, code_execution): dropped. Not
//     modeled in canonical v1 output; per spec comment server tools land in v2.
//   - thinking signature: carried in Reasoning.ProviderData for same-vendor
//     round-trip. Cross-vendor the blob is unusable and dropped on serialize.
//   - pause_turn: maps to StatusIncomplete + IncompleteDetails.Reason="pause_turn".
//   - ping events: silently dropped in stream.
//   - max_tokens: required by Anthropic wire. Dispatch seeds the canonical
//     SamplingParams.MaxTokens from the catalog model's MaxOutputTokens when
//     the caller leaves it unset (see app/httpapi/inference applyOutputDefaults);
//     the 4096 constant here is only a last-resort fallback for models whose
//     catalog entry has no published max.

package anthropic

import (
	"encoding/json"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

const defaultMaxTokensCanonical = 4096

// structuredOutputToolName is the synthetic tool injected to implement
// Output.Format (json_schema / json_object) via the forced-tool trick.
// The double-underscore prefix and "relay" namespace make collisions with
// real caller tools practically impossible.
const structuredOutputToolName = "__relay_structured_output"

// defaultJSONObjectSchema is used for json_object format (and json_schema with
// no schema provided) — the model must return any valid JSON object.
var defaultJSONObjectSchema = json.RawMessage(`{"type":"object"}`)

// AnthropicTranslator implements v1.Translator for the Anthropic Messages API.
type AnthropicTranslator struct{}

// ---- wire types (shared by the request/response/stream files) ----

type anthropicCanonReq struct {
	Model         string                  `json:"model"`
	System        any                     `json:"system,omitempty"` // string, or []block when cache-anchored
	Messages      []anthropicCanonMsg     `json:"messages"`
	Tools         []anthropicCanonTool    `json:"tools,omitempty"`
	ToolChoice    any                     `json:"tool_choice,omitempty"`
	MaxTokens     int                     `json:"max_tokens"`
	Temperature   *float64                `json:"temperature,omitempty"`
	TopP          *float64                `json:"top_p,omitempty"`
	TopK          *int                    `json:"top_k,omitempty"`
	StopSequences []string                `json:"stop_sequences,omitempty"`
	Stream        bool                    `json:"stream,omitempty"`
	Metadata      *anthropicCanonMetadata `json:"metadata,omitempty"`
	Thinking      *anthropicCanonThinking `json:"thinking,omitempty"`
}

type anthropicCanonMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []map[string]any
}

type anthropicCanonTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl map[string]any  `json:"cache_control,omitempty"`
	// EagerInputStreaming disables Anthropic's server-side buffering of
	// input_json_delta. Set on every tool of a streaming request: eager
	// delivery is canonical semantics (OpenAI/Gemini stream args unbuffered),
	// so the buffering is normalized away rather than exposed as a knob. The
	// cost is that accumulated args are no longer server-validated JSON —
	// handleContentBlockStop marks unparseable args StatusIncomplete.
	EagerInputStreaming bool `json:"eager_input_streaming,omitempty"`
}

// anthropicEphemeralCacheControl is the breakpoint marker placed on the last
// block of a cache-anchored section. Anthropic caches everything from the
// start of the prompt up to and including the marked block. ttl is the wire
// ttl tier ("1h") or empty for the 5-minute default.
func anthropicEphemeralCacheControl(ttl string) map[string]any {
	cc := map[string]any{"type": "ephemeral"}
	if ttl != "" {
		cc["ttl"] = ttl
	}
	return cc
}

// anthropicCacheTTL maps canonical CacheConfig.TTL to Anthropic's wire ttl
// tier: anything beyond the default 5 minutes upgrades to the 1-hour tier
// (the only extended tier Anthropic offers; note 1h writes bill 2× vs 1.25×);
// at or under 5m — or unset/unparseable — stays on the default (empty).
func anthropicCacheTTL(cfg *v1.CacheConfig) string {
	if d, ok := cfg.TTLDuration(); ok && d > 5*time.Minute {
		return "1h"
	}
	return ""
}

// withCacheBreakpoint attaches an ephemeral cache_control marker to the last
// content block, coercing string content into a single text block when needed
// (cache_control can only ride on a block, not a bare string).
func withCacheBreakpoint(content any, ttl string) any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return c
		}
		return []map[string]any{{"type": "text", "text": c, "cache_control": anthropicEphemeralCacheControl(ttl)}}
	case []map[string]any:
		if len(c) == 0 {
			return c
		}
		c[len(c)-1]["cache_control"] = anthropicEphemeralCacheControl(ttl)
		return c
	default:
		return content
	}
}

type anthropicCanonMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicCanonThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"` // "summarized" | "omitted" (4.7+ family)
}

// anthropicFullResp is the full Anthropic response shape used by ParseResponse.
type anthropicFullResp struct {
	ID         string               `json:"id"`
	Type       string               `json:"type"`
	Role       string               `json:"role"`
	Model      string               `json:"model"`
	Content    []anthropicRespBlock `json:"content"`
	StopReason string               `json:"stop_reason"`
	StopSeq    *string              `json:"stop_sequence,omitempty"`
	Usage      anthropicFullUsage   `json:"usage"`
}

type anthropicRespBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// thinking block
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// citations
	Citations []anthropicCitation `json:"citations,omitempty"`
}

type anthropicFullUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type anthropicCitation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}
