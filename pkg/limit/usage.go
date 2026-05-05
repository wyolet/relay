package limit

import (
	"bytes"
	"encoding/json"
)

// ParseTokens scans b for an OpenAI-shape usage block and returns total_tokens.
// b may be either a complete JSON object or a streaming SSE chunk containing
// "data: {...}". Returns (0, false) if no usage block is present.
func ParseTokens(b []byte) (int64, bool) {
	b = bytes.TrimRight(b, "\r\n ")
	if len(b) == 0 {
		return 0, false
	}

	// Strip SSE data prefix.
	if bytes.HasPrefix(b, []byte("data: ")) {
		b = b[len("data: "):]
		b = bytes.TrimRight(b, "\r\n ")
	}

	if string(b) == "[DONE]" {
		return 0, false
	}

	var envelope struct {
		Usage *struct {
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return 0, false
	}
	if envelope.Usage == nil {
		return 0, false
	}
	u := envelope.Usage
	if u.TotalTokens > 0 {
		return u.TotalTokens, true
	}
	sum := u.PromptTokens + u.CompletionTokens
	if sum > 0 {
		return sum, true
	}
	return 0, false
}
