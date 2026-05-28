package anthropic

import "encoding/json"

// MessagesRequest is the parsed inbound body for POST /v1/messages.
// Only fields Relay inspects are extracted; the full original body is
// retained in Raw for byte-equivalent upstream forwarding.
// messages content is NOT deep-parsed per hot-path rules.
type MessagesRequest struct {
	Model         string            `json:"model"`
	Stream        bool              `json:"stream,omitempty"`
	MaxTokens     int               `json:"max_tokens,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	StopSequences []string          `json:"stop_sequences,omitempty"`
	System        json.RawMessage   `json:"system,omitempty"` // string | []SystemBlock
	Messages      []json.RawMessage `json:"messages,omitempty"`
	Tools         []Tool            `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	Raw           json.RawMessage   `json:"-"`
}

// Tool is an Anthropic tool definition: {name, description, input_schema}.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// MessagesResponse is the non-streaming response from POST /v1/messages.
type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence *string        `json:"stop_sequence,omitempty"`
	Usage        ResponseUsage  `json:"usage"`
}

// ContentBlock is one element of the Anthropic response content array.
// Type is "text" or "tool_use".
type ContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ResponseUsage carries token counts from the Anthropic response.
type ResponseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}
