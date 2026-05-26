package v1

import "github.com/wyolet/relay/pkg/usage"

// Canonical stream event name constants.
// These six events collapse OpenAI Responses' ~50 event types.
const (
	EventGenerationCreated   = "generation.created"
	EventItemStarted         = "item.started"
	EventItemDelta           = "item.delta"
	EventItemCompleted       = "item.completed"
	EventGenerationCompleted = "generation.completed"
	EventError               = "error"
)

// DeltaKind discriminates the delta payload within an item.delta event.
type DeltaKind string

const (
	DeltaKindText      DeltaKind = "text"
	DeltaKindArguments DeltaKind = "arguments"
	DeltaKindReasoning DeltaKind = "reasoning"
)

// GenerationCreatedEvent is the first event in a stream. Carries the response
// id and model so clients can correlate the stream before any output arrives.
type GenerationCreatedEvent struct {
	ID    string `json:"id"`
	Model string `json:"model"`
}

// ItemStartedEvent signals that a new item has begun. Carries enough metadata
// to identify the item type without the full item body. For FunctionCall items
// Name is populated up front so downstream serializers (e.g. Anthropic's
// tool_use content_block_start, which requires the tool name on the open frame)
// can emit shape-correct wire bytes from the start event alone.
type ItemStartedEvent struct {
	ItemID   string   `json:"item_id"`
	ItemType ItemType `json:"item_type"`
	// Index is the position of this item in the output array.
	Index int `json:"index"`
	// Name is the function name for FunctionCall items. Empty for other kinds.
	Name string `json:"name,omitempty"`
}

// ItemDeltaEvent carries an incremental chunk into the current item.
// Kind discriminates the delta payload: text for message content,
// arguments for function_call arguments, reasoning for reasoning content.
type ItemDeltaEvent struct {
	ItemID string    `json:"item_id"`
	Index  int       `json:"index"`
	Kind   DeltaKind `json:"kind"`
	Delta  string    `json:"delta"`
}

// ItemCompletedEvent signals that the current item has finished.
// Carries the fully assembled item for clients that want the complete value
// without accumulating deltas.
type ItemCompletedEvent struct {
	ItemID string `json:"item_id"`
	Index  int    `json:"index"`
	Item   Item   `json:"item"`
}

// GenerationCompletedEvent is the final event in a stream. Carries the
// terminal response status, finish reason, and aggregated token usage.
type GenerationCompletedEvent struct {
	ID           string       `json:"id"`
	Status       Status       `json:"status"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Usage        usage.Tokens `json:"usage,omitempty"`

	// RelayUsage rides the terminal event when the caller opted into echo
	// (X-WR-Usage: full) on a canonical stream. Relay-produced, nil
	// otherwise — the streaming counterpart of Response.RelayUsage. Only
	// the canonical stream carries it; vendor-shaped streams never do.
	RelayUsage *RelayUsage `json:"relay_usage,omitempty"`
}

// ErrorEvent carries a stream-level failure. After this event no further
// events are emitted.
type ErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
