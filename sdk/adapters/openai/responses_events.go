package openai

// Responses API streaming event name constants.
const (
	ResponsesEventCreated                    = "response.created"
	ResponsesEventInProgress                 = "response.in_progress"
	ResponsesEventOutputItemAdded            = "response.output_item.added"
	ResponsesEventContentPartAdded           = "response.content_part.added"
	ResponsesEventOutputTextDelta            = "response.output_text.delta"
	ResponsesEventOutputTextDone             = "response.output_text.done"
	ResponsesEventContentPartDone            = "response.content_part.done"
	ResponsesEventOutputItemDone             = "response.output_item.done"
	ResponsesEventFunctionCallArgumentsDelta = "response.function_call_arguments.delta"
	ResponsesEventFunctionCallArgumentsDone  = "response.function_call_arguments.done"
	ResponsesEventReasoningTextDelta         = "response.reasoning_text.delta"
	ResponsesEventReasoningTextDone          = "response.reasoning_text.done"
	ResponsesEventRefusalDelta               = "response.refusal.delta"
	ResponsesEventRefusalDone                = "response.refusal.done"
	ResponsesEventCompleted                  = "response.completed"
	ResponsesEventFailed                     = "response.failed"
	ResponsesEventIncomplete                 = "response.incomplete"
	ResponsesEventError                      = "error"
)

// ResponsesSSEEvent is the envelope for a single Responses API server-sent event.
type ResponsesSSEEvent struct {
	Event string `json:"event"` // one of the ResponsesEvent* constants
	Data  any    `json:"data"`
}

// ResponsesCreatedEvent carries the initial response snapshot.
type ResponsesCreatedEvent struct {
	Response *ResponsesResponse `json:"response"`
}

// ResponsesInProgressEvent signals the response has moved to in_progress.
type ResponsesInProgressEvent struct {
	Response *ResponsesResponse `json:"response"`
}

// ResponsesItemAddedEvent signals a new output item has been added.
type ResponsesItemAddedEvent struct {
	OutputIndex int           `json:"output_index"`
	Item        ResponsesItem `json:"item"`
}

// ResponsesContentPartAddedEvent signals a new content part has been added to an item.
type ResponsesContentPartAddedEvent struct {
	ItemID       string        `json:"item_id"`
	OutputIndex  int           `json:"output_index"`
	ContentIndex int           `json:"content_index"`
	Part         ResponsesPart `json:"part"`
}

// ResponsesOutputTextDeltaEvent carries an incremental text delta.
type ResponsesOutputTextDeltaEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// ResponsesOutputTextDoneEvent signals output_text streaming is complete.
type ResponsesOutputTextDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

// ResponsesContentPartDoneEvent signals a content part is done.
type ResponsesContentPartDoneEvent struct {
	ItemID       string        `json:"item_id"`
	OutputIndex  int           `json:"output_index"`
	ContentIndex int           `json:"content_index"`
	Part         ResponsesPart `json:"part"`
}

// ResponsesOutputItemDoneEvent signals an output item is done.
type ResponsesOutputItemDoneEvent struct {
	OutputIndex int           `json:"output_index"`
	Item        ResponsesItem `json:"item"`
}

// ResponsesFunctionCallArgumentsDeltaEvent carries an incremental arguments delta.
type ResponsesFunctionCallArgumentsDeltaEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	CallID      string `json:"call_id"`
	Delta       string `json:"delta"`
}

// ResponsesFunctionCallArgumentsDoneEvent signals function call arguments are complete.
type ResponsesFunctionCallArgumentsDoneEvent struct {
	ItemID      string `json:"item_id"`
	OutputIndex int    `json:"output_index"`
	CallID      string `json:"call_id"`
	Arguments   string `json:"arguments"`
}

// ResponsesReasoningTextDeltaEvent carries an incremental reasoning text delta.
type ResponsesReasoningTextDeltaEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// ResponsesReasoningTextDoneEvent signals reasoning text streaming is complete.
type ResponsesReasoningTextDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

// ResponsesRefusalDeltaEvent carries an incremental refusal text delta.
type ResponsesRefusalDeltaEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// ResponsesRefusalDoneEvent signals refusal text streaming is complete.
type ResponsesRefusalDoneEvent struct {
	ItemID       string `json:"item_id"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Refusal      string `json:"refusal"`
}

// ResponsesCompletedEvent carries the final completed response.
type ResponsesCompletedEvent struct {
	Response *ResponsesResponse `json:"response"`
}

// ResponsesFailedEvent carries the failed response.
type ResponsesFailedEvent struct {
	Response *ResponsesResponse `json:"response"`
}

// ResponsesIncompleteEvent carries the incomplete response.
type ResponsesIncompleteEvent struct {
	Response *ResponsesResponse `json:"response"`
}

// ResponsesErrorEvent carries a top-level stream error.
type ResponsesErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
}
