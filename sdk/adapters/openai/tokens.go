package openai

import (
	"bytes"
	"encoding/json"

	"github.com/wyolet/relay/sdk/usage"
)

// ExtractTokens reads OpenAI usage from a response body. It accepts either a
// single non-streaming JSON object or a complete streaming SSE body. For
// streaming (stream_options.include_usage: true) the usage block appears only
// in the final chunk, so we walk every `data:` frame and keep the last one
// that carries usage.
func ExtractTokens(body []byte) usage.Tokens {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		return extractTokensObject(trimmed)
	}
	var last usage.Tokens
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		if frame := extractTokensObject(payload); frame != nil {
			last = frame
		}
	}
	return last
}

// extractTokensObject reads a usage block out of a single JSON object (a
// non-streaming response or one SSE data frame).
func extractTokensObject(body []byte) usage.Tokens {
	var resp struct {
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
				AudioTokens  int64 `json:"audio_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
			CompletionTokensDetails *struct {
				ReasoningTokens          int64 `json:"reasoning_tokens"`
				AudioTokens              int64 `json:"audio_tokens"`
				AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
				RejectedPredictionTokens int64 `json:"rejected_prediction_tokens"`
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
	cached := int64(0)
	if d := resp.Usage.PromptTokensDetails; d != nil {
		cached = d.CachedTokens
	}
	if v := resp.Usage.PromptTokens - cached; v > 0 {
		t["input"] = v
	}
	if v := resp.Usage.CompletionTokens; v > 0 {
		t["output"] = v
	}
	if d := resp.Usage.PromptTokensDetails; d != nil {
		if cached > 0 {
			t["cache_read"] = cached
		}
		if d.AudioTokens > 0 {
			t["audio_input"] = d.AudioTokens
		}
	}
	if d := resp.Usage.CompletionTokensDetails; d != nil {
		if d.ReasoningTokens > 0 {
			t["reasoning"] = d.ReasoningTokens
		}
		if d.AudioTokens > 0 {
			t["audio_output"] = d.AudioTokens
		}
		if d.AcceptedPredictionTokens > 0 {
			t["accepted_prediction"] = d.AcceptedPredictionTokens
		}
		if d.RejectedPredictionTokens > 0 {
			t["rejected_prediction"] = d.RejectedPredictionTokens
		}
	}
	return t
}
