package openai

import v1 "github.com/wyolet/relay/sdk/v1"

// ResponsesItemType is the wire discriminator for the Responses API Input/Output item union.
type ResponsesItemType string

const (
	ResponsesItemTypeMessage              ResponsesItemType = "message"
	ResponsesItemTypeFunctionCall         ResponsesItemType = "function_call"
	ResponsesItemTypeFunctionCallOutput   ResponsesItemType = "function_call_output"
	ResponsesItemTypeReasoning            ResponsesItemType = "reasoning"
	ResponsesItemTypeCustomToolCall       ResponsesItemType = "custom_tool_call"
	ResponsesItemTypeCustomToolCallOutput ResponsesItemType = "custom_tool_call_output"
)

// responsesCanonicalItemType maps a wire item type to its canonical one. Everything else (hosted-tool calls) is carried as a ResponsesRawItem and dropped cross-shape, so the streaming path must not emit a canonical item lifecycle for it. A custom tool call is a function call in canonical — the freeform input becomes the lowered `input` argument.
func responsesCanonicalItemType(t ResponsesItemType) (v1.ItemType, bool) {
	switch t {
	case ResponsesItemTypeMessage:
		return v1.ItemTypeMessage, true
	case ResponsesItemTypeFunctionCall, ResponsesItemTypeCustomToolCall:
		return v1.ItemTypeFunctionCall, true
	case ResponsesItemTypeFunctionCallOutput, ResponsesItemTypeCustomToolCallOutput:
		return v1.ItemTypeFunctionCallOutput, true
	case ResponsesItemTypeReasoning:
		return v1.ItemTypeReasoning, true
	default:
		return "", false
	}
}

// ResponsesPartType is the wire discriminator for the Responses API Content part union.
type ResponsesPartType string

const (
	ResponsesPartTypeInputText  ResponsesPartType = "input_text"
	ResponsesPartTypeInputImage ResponsesPartType = "input_image"
	ResponsesPartTypeInputFile  ResponsesPartType = "input_file"
	ResponsesPartTypeOutputText ResponsesPartType = "output_text"
	ResponsesPartTypeRefusal    ResponsesPartType = "refusal"
)

// ResponsesToolType is the wire discriminator for the Responses API Tool union.
type ResponsesToolType string

const (
	ResponsesToolTypeFunction  ResponsesToolType = "function"
	ResponsesToolTypeCustom    ResponsesToolType = "custom"
	ResponsesToolTypeNamespace ResponsesToolType = "namespace"
)

// ResponsesRole enumerates valid message roles in the Responses API.
type ResponsesRole string

const (
	ResponsesRoleUser      ResponsesRole = "user"
	ResponsesRoleAssistant ResponsesRole = "assistant"
	ResponsesRoleSystem    ResponsesRole = "system"
	ResponsesRoleDeveloper ResponsesRole = "developer"
)

// ResponsesStatus is the item or response lifecycle state.
type ResponsesStatus string

const (
	ResponsesStatusCompleted  ResponsesStatus = "completed"
	ResponsesStatusIncomplete ResponsesStatus = "incomplete"
	ResponsesStatusFailed     ResponsesStatus = "failed"
	ResponsesStatusInProgress ResponsesStatus = "in_progress"
)

// The Responses API has no finish_reason field: terminal state is carried by
// status + incomplete_details.reason. Canonical finish_reason is derived from
// those (see responsesCanonicalFinishReason / canonicalResponsesStatus).

// ResponsesItem is a sealed interface for elements of the Responses API Input or Output array.
// Valid concrete types: *ResponsesMessage, *ResponsesFunctionCall,
// *ResponsesFunctionCallOutput, *ResponsesReasoning, *ResponsesCustomToolCall,
// *ResponsesCustomToolCallOutput, and *ResponsesRawItem.
// External packages may not implement this interface.
type ResponsesItem interface {
	isResponsesItem()
	ResponsesItemType() ResponsesItemType
}

// ResponsesPart is a sealed interface for elements of a Responses API message's Content array.
// Valid concrete types: *ResponsesTextPart, *ResponsesImagePart, *ResponsesFilePart (input),
// *ResponsesOutputTextPart, *ResponsesRefusalPart (output).
type ResponsesPart interface {
	isResponsesPart()
	ResponsesPartType() ResponsesPartType
}

// ResponsesTool is a sealed interface for elements of the Responses API Tools array.
// Valid concrete types: *ResponsesFunctionTool, *ResponsesCustomTool,
// *ResponsesNamespaceTool, and *ResponsesRawTool.
type ResponsesTool interface {
	isResponsesTool()
	ResponsesToolType() ResponsesToolType
}

// ResponsesAnnotation is a sealed interface for citation annotations on output text.
// Concrete types: *ResponsesURLCitationAnnotation, *ResponsesFileCitationAnnotation,
// *ResponsesRawAnnotation.
type ResponsesAnnotation interface {
	isResponsesAnnotation()
	ResponsesAnnotationType() string
}
