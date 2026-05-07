package openai

import (
	"encoding/json"

	"github.com/wyolet/relay/pkg/usage"
)

// ExtractTokens reads OpenAI usage from a response body chunk and emits
// a Tokens map. Returns nil if the body has no usage block (e.g. mid-stream
// SSE chunks before the final one).
//
// Maps OpenAI's fields to our convention:
//
//	prompt_tokens                                              -> input
//	completion_tokens                                          -> output
//	total_tokens                                               -> (skipped; computed from sum)
//	prompt_tokens_details.cached_tokens                        -> cache_read
//	completion_tokens_details.reasoning_tokens                 -> reasoning
//
// This also handles the streaming case where usage appears in the final
// chunk only (when stream_options.include_usage: true). The chunk shape
// is {... "usage": {...}} at the message level — same path, same parser.
func ExtractTokens(body []byte) usage.Tokens {
	var resp struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details,omitempty"`
		} `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil
	}
	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 {
		return nil
	}

	t := usage.Tokens{}
	if v := resp.Usage.PromptTokens; v > 0 {
		t["input"] = v
	}
	if v := resp.Usage.CompletionTokens; v > 0 {
		t["output"] = v
	}
	if d := resp.Usage.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
		t["cache_read"] = d.CachedTokens
	}
	if d := resp.Usage.CompletionTokensDetails; d != nil && d.ReasoningTokens > 0 {
		t["reasoning"] = d.ReasoningTokens
	}
	return t
}
