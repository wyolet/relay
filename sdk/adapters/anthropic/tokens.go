package anthropic

import (
	"bytes"
	"encoding/json"

	"github.com/wyolet/relay/sdk/usage"
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
// It accepts either a single non-streaming JSON object or a complete
// streaming SSE body. For streaming, the input-side counters
// (input/cache_creation/cache_read) appear ONLY in message_start, while
// output_tokens reaches its final value in message_delta — so a reader that
// only inspects the last event captures cache_read but silently loses
// cache_creation. We therefore walk every `data:` frame and merge by max per
// key: every Anthropic usage counter is cumulative or appears once, so max
// never double-counts and never drops a message_start-only field.
func ExtractTokens(body []byte) usage.Tokens {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return nil
	}

	// Single JSON object: non-streaming response, or a single SSE data
	// payload already split out by the caller.
	if trimmed[0] == '{' {
		return extractFromObject(trimmed)
	}

	// SSE stream: accumulate usage across all data: frames.
	var acc usage.Tokens
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		frame := extractFromObject(payload)
		if frame == nil {
			continue
		}
		if acc == nil {
			acc = usage.Tokens{}
		}
		for k, v := range frame {
			if v > acc[k] {
				acc[k] = v
			}
		}
	}
	return acc
}

// extractFromObject reads a usage block out of a single JSON object — either a
// non-streaming response / message_delta (top-level "usage") or a
// message_start SSE event (usage nested under "message").
func extractFromObject(body []byte) usage.Tokens {
	var obj struct {
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
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}

	// Prefer top-level usage; fall back to message.usage (message_start event).
	u := obj.Usage
	if u == nil && obj.Message != nil {
		mu := obj.Message.Usage
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
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
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
