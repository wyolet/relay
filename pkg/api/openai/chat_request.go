package openai

import "encoding/json"

// ChatRequest is the parsed inbound body for POST /v1/chat/completions.
// Only fields Relay inspects are extracted; the full original body is
// retained in Raw for byte-equivalent upstream forwarding.
type ChatRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream,omitempty"`
	User     string            `json:"user,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Messages []json.RawMessage `json:"messages,omitempty"`
	Raw      json.RawMessage   `json:"-"`
}
