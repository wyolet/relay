package ratelimit

import (
	"bytes"
	"encoding/json"
)

// TokenBlock holds parsed token counts from a provider response.
type TokenBlock struct {
	Prompt     int64
	Completion int64
	Total      int64
}

// ParseTokensFull scans b for an OpenAI-shape usage block and returns all
// three token counts. Returns zero-value and false if no usage block is found.
func ParseTokensFull(b []byte) (TokenBlock, bool) {
	b = bytes.TrimRight(b, "\r\n ")
	if len(b) == 0 {
		return TokenBlock{}, false
	}
	if bytes.HasPrefix(b, []byte("data: ")) {
		b = b[len("data: "):]
		b = bytes.TrimRight(b, "\r\n ")
	}
	if string(b) == "[DONE]" {
		return TokenBlock{}, false
	}
	var envelope struct {
		Usage *struct {
			TotalTokens      int64 `json:"total_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &envelope); err != nil {
		return TokenBlock{}, false
	}
	if envelope.Usage == nil {
		return TokenBlock{}, false
	}
	u := envelope.Usage
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	if total == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return TokenBlock{}, false
	}
	return TokenBlock{Prompt: u.PromptTokens, Completion: u.CompletionTokens, Total: total}, true
}

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
