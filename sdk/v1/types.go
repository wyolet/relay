package v1

// ItemType is the wire discriminator for the input/output item union.
type ItemType string

const (
	ItemTypeMessage            ItemType = "message"
	ItemTypeFunctionCall       ItemType = "function_call"
	ItemTypeFunctionCallOutput ItemType = "function_call_output"
	ItemTypeReasoning          ItemType = "reasoning"
)

// PartType is the wire discriminator for the content part union.
type PartType string

const (
	PartTypeInputText  PartType = "input_text"
	PartTypeInputImage PartType = "input_image"
	PartTypeInputFile  PartType = "input_file"
	PartTypeOutputText PartType = "output_text"
)

// ToolType is the wire discriminator for the tool union.
type ToolType string

const (
	ToolTypeFunction ToolType = "function"
	ToolTypeServer   ToolType = "server"
	ToolTypeMCP      ToolType = "mcp"
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
	// FinishReasonRefusal: refusal is a stop reason, not a part type.
	// The refusal text appears in a normal message item's text content.
	FinishReasonRefusal FinishReason = "refusal"
)

// Item is a sealed interface for elements of the Input or Output array.
// Valid concrete types: *Message, *FunctionCall, *FunctionCallOutput, *Reasoning.
// External packages may not implement this interface.
type Item interface {
	isItem()
	ItemType() ItemType
}

// Part is a sealed interface for elements of a message's Content array.
// Valid concrete types: *TextPart, *ImagePart, *FilePart (input),
// *OutputTextPart (output).
// External packages may not implement this interface.
type Part interface {
	isPart()
	PartType() PartType
}

// Tool is a sealed interface for elements of the Tools array.
// Valid concrete types: *FunctionTool, *ServerTool, *MCPTool.
// External packages may not implement this interface.
type Tool interface {
	isTool()
	ToolType() ToolType
}

// Annotation is a sealed interface for citation annotations on output text.
// Concrete types: *URLCitationAnnotation, *FileCitationAnnotation, *RawAnnotation.
type Annotation interface {
	isAnnotation()
	AnnotationType() string
}
