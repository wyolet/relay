package cctranslator

import (
	"time"

	"github.com/wyolet/relay/pkg/adapters/openai"
	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

// CCToResponse converts a Chat Completions non-streaming response to a
// Responses API Response object.
//
// req is the original Responses API request; its fields are echoed into the
// response per the OpenAI spec. Pass nil for tests that don't need echo fields.
// modelOverride, when non-empty, replaces the model field from the CC response
// (useful when the upstream echoes a deployment alias rather than the logical
// model name).
func CCToResponse(req *responses.Request, cc *openai.ChatResponse, modelOverride string) (*responses.Response, error) {
	model := cc.Model
	if modelOverride != "" {
		model = modelOverride
	}

	resp := &responses.Response{
		ID:        cc.ID,
		Object:    "response",
		CreatedAt: cc.Created,
		Model:     model,
	}

	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}

	if cc.Usage != nil {
		resp.Usage = translateUsage(cc.Usage)
	}

	var firstChoice *openai.Choice
	if len(cc.Choices) > 0 {
		firstChoice = &cc.Choices[0]
	}

	resp.Status, resp.FinishReason, resp.IncompleteDetails = mapFinishReason(firstChoice)

	if firstChoice != nil {
		resp.Output = buildOutput(cc.ID, firstChoice)
	}

	responses.EchoRequest(resp, req)
	return resp, nil
}

// mapFinishReason maps a CC finish_reason to Responses status + finish_reason.
func mapFinishReason(choice *openai.Choice) (responses.Status, responses.FinishReason, *responses.IncompleteDetails) {
	if choice == nil {
		return responses.StatusCompleted, responses.FinishReasonStop, nil
	}
	switch choice.FinishReason {
	case "stop":
		return responses.StatusCompleted, responses.FinishReasonStop, nil
	case "length":
		return responses.StatusIncomplete, responses.FinishReasonLength, &responses.IncompleteDetails{Reason: "max_output_tokens"}
	case "tool_calls":
		return responses.StatusCompleted, responses.FinishReasonToolCalls, nil
	case "content_filter":
		return responses.StatusCompleted, responses.FinishReasonContentFilter, nil
	default:
		return responses.StatusCompleted, responses.FinishReasonStop, nil
	}
}

// buildOutput constructs the Responses output []Item from a CC choice.
// Ordering: reasoning item (if present) → message item → function_call items.
// ccID is the CC response id, used to synthesize stable per-item ids.
func buildOutput(ccID string, ch *openai.Choice) []responses.Item {
	var items []responses.Item
	msg := ch.Message

	// Message item — emit when there is text content or a refusal.
	var textContent string
	if msg.Content != nil {
		textContent = *msg.Content
	}
	refusal := ""
	if msg.Refusal != nil {
		refusal = *msg.Refusal
	}

	if textContent != "" || refusal != "" || len(msg.ToolCalls) == 0 {
		msgItem := &responses.Message{
			// Spec marks id, role, content, status, type as required on
			// OutputMessage. Synthesize id from the CC response id + choice
			// index so different choices get distinct ids; mark completed
			// since this is the final non-streaming response.
			ID:     "msg_" + ccID,
			Role:   responses.RoleAssistant,
			Status: responses.StatusCompleted,
		}
		if textContent != "" {
			msgItem.Content = []responses.Part{&responses.OutputTextPart{Text: textContent}}
		}
		if refusal != "" {
			msgItem.Content = append(msgItem.Content, &responses.RefusalPart{Refusal: refusal})
		}
		items = append(items, msgItem)
	}

	// FunctionCall items — one per tool call.
	for _, tc := range msg.ToolCalls {
		items = append(items, &responses.FunctionCall{
			ID:        tc.ID,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
			Status:    responses.StatusCompleted,
		})
	}

	return items
}

// translateUsage maps CC Usage → Responses Usage.
// InputTokensDetails and OutputTokensDetails are always populated (spec required).
func translateUsage(u *openai.Usage) *responses.Usage {
	ru := &responses.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.PromptDetails != nil {
		ru.InputTokensDetails = responses.InputDeets{CachedTokens: u.PromptDetails.CachedTokens}
	}
	if u.CompletionDetails != nil {
		ru.OutputTokensDetails = responses.OutputDeets{ReasoningTokens: u.CompletionDetails.ReasoningTokens}
	}
	return ru
}
