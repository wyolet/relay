package responses

// Streaming event name constants.
const (
	EventCreated                      = "response.created"
	EventInProgress                   = "response.in_progress"
	EventOutputItemAdded              = "response.output_item.added"
	EventContentPartAdded             = "response.content_part.added"
	EventOutputTextDelta              = "response.output_text.delta"
	EventOutputTextDone               = "response.output_text.done"
	EventContentPartDone              = "response.content_part.done"
	EventOutputItemDone               = "response.output_item.done"
	EventFunctionCallArgumentsDelta   = "response.function_call_arguments.delta"
	EventFunctionCallArgumentsDone    = "response.function_call_arguments.done"
	EventReasoningTextDelta           = "response.reasoning_text.delta"
	EventReasoningTextDone            = "response.reasoning_text.done"
	EventRefusalDelta                 = "response.refusal.delta"
	EventRefusalDone                  = "response.refusal.done"
	EventCompleted                    = "response.completed"
	EventFailed                       = "response.failed"
	EventIncomplete                   = "response.incomplete"
	EventError                        = "error"
)

// SSEEvent is the envelope for a single server-sent event.
type SSEEvent struct {
	Event string `json:"event"` // one of the Event* constants
	Data  any    `json:"data"`
}

// CreatedEvent carries the initial response snapshot.
type CreatedEvent struct {
	Response *Response `json:"response"`
}

// InProgressEvent signals the response has moved to in_progress.
type InProgressEvent struct {
	Response *Response `json:"response"`
}

// ItemAddedEvent signals a new output item has been added.
type ItemAddedEvent struct {
	OutputIndex int  `json:"output_index"`
	Item        Item `json:"item"`
}

// ContentPartAddedEvent signals a new content part has been added to an item.
type ContentPartAddedEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Part         Part   `json:"part"`
}

// OutputTextDeltaEvent carries an incremental text delta.
type OutputTextDeltaEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// OutputTextDoneEvent signals output_text streaming is complete.
type OutputTextDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

// ContentPartDoneEvent signals a content part is done.
type ContentPartDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Part         Part   `json:"part"`
}

// OutputItemDoneEvent signals an output item is done.
type OutputItemDoneEvent struct {
	OutputIndex int  `json:"output_index"`
	Item        Item `json:"item"`
}

// FunctionCallArgumentsDeltaEvent carries an incremental arguments delta.
type FunctionCallArgumentsDeltaEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	CallID      string `json:"call_id"`
	Delta       string `json:"delta"`
}

// FunctionCallArgumentsDoneEvent signals function call arguments are complete.
type FunctionCallArgumentsDoneEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	CallID      string `json:"call_id"`
	Arguments   string `json:"arguments"`
}

// ReasoningTextDeltaEvent carries an incremental reasoning text delta.
type ReasoningTextDeltaEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// ReasoningTextDoneEvent signals reasoning text streaming is complete.
type ReasoningTextDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

// RefusalDeltaEvent carries an incremental refusal text delta.
type RefusalDeltaEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// RefusalDoneEvent signals refusal text streaming is complete.
type RefusalDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Refusal      string `json:"refusal"`
}

// CompletedEvent carries the final completed response.
type CompletedEvent struct {
	Response *Response `json:"response"`
}

// FailedEvent carries the failed response.
type FailedEvent struct {
	Response *Response `json:"response"`
}

// IncompleteEvent carries the incomplete response.
type IncompleteEvent struct {
	Response *Response `json:"response"`
}

// ErrorEvent carries a top-level stream error.
type ErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}
