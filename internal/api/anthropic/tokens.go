package anthropic

import (
	"encoding/json"

	"github.com/wyolet/relay/internal/usage"
)

// ExtractTokens reads Anthropic usage from a /v1/messages response body.
// Maps Anthropic's fields:
//
//	input_tokens                  -> input
//	output_tokens                 -> output
//	cache_creation_input_tokens   -> cache_creation
//	cache_read_input_tokens       -> cache_read
//	server_tool_use.input_tokens  -> server_tool_use_input
//	server_tool_use.output_tokens -> server_tool_use_output
//
// For streaming, usage appears in message_start (input tokens) and
// message_delta (output tokens). Calling ExtractTokens on each SSE data
// payload extracts usage from whichever chunks carry a usage block.
// The pipeline accumulates results via Tokens.Add.
func ExtractTokens(body []byte) usage.Tokens {
	// Non-streaming: usage at top level.
	// Streaming message_start: usage inside message.usage.
	// Streaming message_delta: usage at top level.
	// We try both paths.

	// Try top-level usage first.
	var topLevel struct {
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			ServerToolUse            *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"server_tool_use,omitempty"`
		} `json:"usage,omitempty"`
		// message_start wraps usage in message.usage
		Message *struct {
			Usage *struct {
				InputTokens              int64 `json:"input_tokens"`
				OutputTokens             int64 `json:"output_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			} `json:"usage,omitempty"`
		} `json:"message,omitempty"`
	}
	if err := json.Unmarshal(body, &topLevel); err != nil {
		return nil
	}

	// Prefer top-level usage; fall back to message.usage (message_start SSE event).
	u := topLevel.Usage
	if u == nil && topLevel.Message != nil {
		mu := topLevel.Message.Usage
		if mu == nil {
			return nil
		}
		t := usage.Tokens{}
		if mu.InputTokens > 0 {
			t["input"] = mu.InputTokens
		}
		if mu.OutputTokens > 0 {
			t["output"] = mu.OutputTokens
		}
		if mu.CacheCreationInputTokens > 0 {
			t["cache_creation"] = mu.CacheCreationInputTokens
		}
		if mu.CacheReadInputTokens > 0 {
			t["cache_read"] = mu.CacheReadInputTokens
		}
		if len(t) == 0 {
			return nil
		}
		return t
	}
	if u == nil {
		return nil
	}
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		return nil
	}

	t := usage.Tokens{}
	if u.InputTokens > 0 {
		t["input"] = u.InputTokens
	}
	if u.OutputTokens > 0 {
		t["output"] = u.OutputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		t["cache_creation"] = u.CacheCreationInputTokens
	}
	if u.CacheReadInputTokens > 0 {
		t["cache_read"] = u.CacheReadInputTokens
	}
	if s := u.ServerToolUse; s != nil {
		if s.InputTokens > 0 {
			t["server_tool_use_input"] = s.InputTokens
		}
		if s.OutputTokens > 0 {
			t["server_tool_use_output"] = s.OutputTokens
		}
	}
	return t
}
