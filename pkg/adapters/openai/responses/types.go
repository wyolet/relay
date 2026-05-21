// Package responses provides Go types and a parser for the OpenAI Responses API.
// It covers POST /v1/responses request bodies, response objects, and streaming
// event payloads.
//
// Scope: types + parsing only. No translators, no routes, no pipeline wiring.
//
// Discriminated unions (Item, Part, Tool) use sealed interfaces — an unexported
// marker method prevents external implementations and makes exhaustive type
// switches safe.
package responses

// ItemType is the wire discriminator for the Input/Output item union.
type ItemType string

const (
	ItemTypeMessage            ItemType = "message"
	ItemTypeFunctionCall       ItemType = "function_call"
	ItemTypeFunctionCallOutput ItemType = "function_call_output"
	ItemTypeReasoning          ItemType = "reasoning"
)

// PartType is the wire discriminator for the Content part union.
type PartType string

const (
	PartTypeInputText  PartType = "input_text"
	PartTypeInputImage PartType = "input_image"
	PartTypeInputFile  PartType = "input_file"
	PartTypeOutputText PartType = "output_text"
	PartTypeRefusal    PartType = "refusal"
)

// ToolType is the wire discriminator for the Tool union.
type ToolType string

const (
	ToolTypeFunction ToolType = "function"
)

// Role enumerates valid message roles.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
)

// Status is the item or response lifecycle state.
type Status string

const (
	StatusCompleted  Status = "completed"
	StatusIncomplete Status = "incomplete"
	StatusFailed     Status = "failed"
	StatusInProgress Status = "in_progress"
)

// FinishReason explains why generation stopped.
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"
	FinishReasonLength        FinishReason = "length"
	FinishReasonContentFilter FinishReason = "content_filter"
	FinishReasonToolCalls     FinishReason = "tool_calls"
)

// Item is a sealed interface for elements of the Input or Output array.
// The only valid concrete types are *Message, *FunctionCall, *FunctionCallOutput,
// and *Reasoning. External packages may not implement this interface.
type Item interface {
	isItem()
	// ItemType returns the wire type discriminator.
	ItemType() ItemType
}

// Part is a sealed interface for elements of a message's Content array.
// Valid concrete types: *TextPart, *ImagePart, *FilePart (input),
// *OutputTextPart, *RefusalPart (output).
type Part interface {
	isPart()
	// PartType returns the wire type discriminator.
	PartType() PartType
}

// Tool is a sealed interface for elements of the Tools array.
// The only valid concrete type is *FunctionTool.
type Tool interface {
	isTool()
	// ToolType returns the wire type discriminator.
	ToolType() ToolType
}

// Annotation is a sealed interface for citation annotations on output text.
// Concrete types: *URLCitationAnnotation, *FileCitationAnnotation, *RawAnnotation.
type Annotation interface {
	isAnnotation()
	// AnnotationType returns the wire type discriminator.
	AnnotationType() string
}
