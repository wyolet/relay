package anthropic

import "encoding/json"

// MessagesRequest is the parsed inbound body for POST /v1/messages.
// Only fields Relay inspects are extracted; the full original body is
// retained in Raw for byte-equivalent upstream forwarding.
// messages content is NOT deep-parsed per hot-path rules.
type MessagesRequest struct {
	Model     string            `json:"model"`
	Stream    bool              `json:"stream,omitempty"`
	MaxTokens int               `json:"max_tokens,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Messages  []json.RawMessage `json:"messages,omitempty"`
	Raw       json.RawMessage   `json:"-"`
}
