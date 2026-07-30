package openai

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ParseResponse decodes a CC non-streaming response body into canonical *v1.Response.
func (CCTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	var cc ChatResponse
	if err := json.Unmarshal(body, &cc); err != nil {
		return nil, fmt.Errorf("cc parse_response: %w", err)
	}

	resp := &v1.Response{
		ID:        cc.ID,
		Object:    "response",
		CreatedAt: cc.Created,
		Model:     cc.Model,
	}
	if resp.CreatedAt == 0 {
		resp.CreatedAt = time.Now().Unix()
	}

	if cc.Usage != nil {
		resp.Usage = ccUsageToCanonical(cc.Usage)
	}

	if len(cc.Choices) == 0 {
		resp.Status = v1.StatusCompleted
		resp.FinishReason = v1.FinishReasonStop
		return resp, nil
	}

	ch := &cc.Choices[0]
	resp.Status, resp.FinishReason, resp.IncompleteDetails = ccFinishReasonToCanonical(ch.FinishReason)
	resp.Output = ccChoiceToCanonicalOutput(cc.ID, ch)

	return resp, nil
}

// SerializeResponse encodes a canonical *v1.Response to a CC response body.
// req is unused (CC doesn't require request-echo). May be nil.
// SerializeResponse encodes a canonical *v1.Response to a CC response body.
// req is unused (CC doesn't require request-echo). May be nil.
func (CCTranslator) SerializeResponse(resp *v1.Response, _ *v1.Request) ([]byte, error) {
	// CC-5: surface errors as an OpenAI error body instead of a silent empty choices response.
	if resp.Error != nil {
		type ccError struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		}
		type ccErrorBody struct {
			Error ccError `json:"error"`
		}
		return json.Marshal(ccErrorBody{Error: ccError{
			Message: resp.Error.Message,
			Type:    resp.Error.Code,
			Code:    resp.Error.Code,
		}})
	}

	cc := ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.CreatedAt,
		Model:   resp.Model,
	}
	if len(resp.Usage) > 0 {
		cc.Usage = canonicalUsageToCC(resp.Usage)
	}

	// Build choices from output items.
	var msg ChatResponseMessage
	msg.Role = "assistant"
	var toolCalls []ToolCall
	var finishReason string

	switch resp.FinishReason {
	case v1.FinishReasonStop:
		finishReason = "stop"
	case v1.FinishReasonLength:
		finishReason = "length"
	case v1.FinishReasonToolCalls:
		finishReason = "tool_calls"
	case v1.FinishReasonContentFilter:
		finishReason = "content_filter"
	case v1.FinishReasonRefusal:
		finishReason = "stop"
	default:
		finishReason = "stop"
	}

	var textBuf []byte
	var refusalText string

	for _, item := range resp.Output {
		switch v := item.(type) {
		case *v1.Message:
			for _, p := range v.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					textBuf = append(textBuf, tp.Text...)
				case *v1.TextPart:
					textBuf = append(textBuf, tp.Text...)
				}
			}
			// If finish_reason was refusal, the refusal text is the message content.
			if resp.FinishReason == v1.FinishReasonRefusal {
				refusalText = string(textBuf)
				textBuf = nil
			}
		case *v1.FunctionCall:
			toolCalls = append(toolCalls, ToolCall{
				ID:   v.CallID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      v.Name,
					Arguments: v.Arguments,
				},
			})
		case *v1.Reasoning:
			// Echo reasoning under its original wire field, preserved in
			// provider_data (Ollama "reasoning" / o-series "reasoning_content").
			rt := v.Content
			if rt == "" {
				for _, st := range v.Summary {
					rt += st.Text
				}
			}
			if rt != "" {
				if ccReasoningField(v.ProviderData) == ccReasoningFieldOllama {
					msg.Reasoning = rt
				} else {
					msg.ReasoningContent = rt
				}
			}
		}
	}

	if refusalText != "" {
		msg.Refusal = &refusalText
	} else if len(textBuf) > 0 {
		s := string(textBuf)
		msg.Content = &s
	} else if len(toolCalls) == 0 {
		s := ""
		msg.Content = &s
	}
	msg.ToolCalls = toolCalls

	cc.Choices = []Choice{{
		Index:        0,
		Message:      msg,
		FinishReason: finishReason,
	}}

	return json.Marshal(cc)
}

// ccFinishReasonToCanonical maps a CC finish_reason string to canonical status/finish/incomplete.
func ccFinishReasonToCanonical(reason string) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	switch reason {
	case "stop":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "length":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_output_tokens"}
	case "tool_calls":
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	case "content_filter":
		return v1.StatusCompleted, v1.FinishReasonContentFilter, nil
	default:
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	}
}

// ccChoiceToCanonicalOutput converts a CC Choice to canonical []v1.Item.
// ccChoiceToCanonicalOutput converts a CC Choice to canonical []v1.Item.
// CC-2: map message-level annotations to OutputTextPart.Annotations.
func ccChoiceToCanonicalOutput(ccID string, ch *Choice) []v1.Item {
	var items []v1.Item
	msg := ch.Message

	var textContent string
	if msg.Content != nil {
		textContent = *msg.Content
	}
	refusal := ""
	if msg.Refusal != nil {
		refusal = *msg.Refusal
	}

	// Reasoning leads the output. Ollama emits "reasoning", o-series/DeepSeek
	// emit "reasoning_content"; canonical maps both to one Reasoning.Content
	// and provider_data preserves which field carried it.
	rtext, rfield := msg.ReasoningContent, ccReasoningFieldStd
	if rtext == "" && msg.Reasoning != "" {
		rtext, rfield = msg.Reasoning, ccReasoningFieldOllama
	}
	if rtext != "" {
		items = append(items, &v1.Reasoning{
			ID:           "rs_" + ccID,
			Content:      rtext,
			Summary:      []v1.SummaryText{{Text: rtext}},
			Status:       v1.StatusCompleted,
			ProviderData: ccReasoningProviderDataJSON(rfield),
		})
	}

	// Emit message item if there is text or refusal content, or no tool calls.
	if textContent != "" || refusal != "" || len(msg.ToolCalls) == 0 {
		msgItem := &v1.Message{
			ID:     "msg_" + ccID,
			Role:   v1.RoleAssistant,
			Status: v1.StatusCompleted,
		}
		if textContent != "" {
			part := &v1.OutputTextPart{Text: textContent}
			for _, ann := range msg.Annotations {
				if ann.Type == "url_citation" {
					part.Annotations = append(part.Annotations, &v1.URLCitationAnnotation{
						StartIndex: ann.URLCitation.StartIndex,
						EndIndex:   ann.URLCitation.EndIndex,
						URL:        ann.URLCitation.URL,
						Title:      ann.URLCitation.Title,
					})
				}
			}
			msgItem.Content = []v1.Part{part}
		}
		// Canonical rule 9: refusal text is in normal message content with finish_reason="refusal".
		if refusal != "" {
			msgItem.Content = append(msgItem.Content, &v1.OutputTextPart{Text: refusal})
		}
		items = append(items, msgItem)
	}

	// FunctionCall items.
	for _, tc := range msg.ToolCalls {
		items = append(items, &v1.FunctionCall{
			ID:        tc.ID,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
			Status:    v1.StatusCompleted,
		})
	}

	return items
}

// ccUsageToCanonical maps CC's Usage block to the canonical
// orthogonal-meter Tokens map.
//
// OpenAI's prompt_tokens INCLUDES cached tokens; canonical "input"
// means non-cached input only (consistent with Anthropic semantics).
// We subtract cached_tokens from prompt_tokens so input + cache_read
// reconstructs the prompt total. Note the output side is NOT fully
// orthogonal: reasoning/audio_output/predictions are sub-breakdowns of
// completion, so Tokens.Sum() over this map double-counts them. The
// honest request total is input + cache_read + output (== prompt +
// completion), never Sum() — see canonicalUsageToCC.
func ccUsageToCanonical(u *Usage) usage.Tokens {
	if u == nil {
		return nil
	}
	t := usage.Tokens{}
	cached := int64(0)
	if u.PromptDetails != nil {
		cached = int64(u.PromptDetails.CachedTokens)
	}
	if v := int64(u.PromptTokens) - cached; v > 0 {
		t["input"] = v
	}
	if u.CompletionTokens > 0 {
		t["output"] = int64(u.CompletionTokens)
	}
	if cached > 0 {
		t["cache_read"] = cached
	}
	if u.PromptDetails != nil && u.PromptDetails.AudioTokens > 0 {
		t["audio_input"] = int64(u.PromptDetails.AudioTokens)
	}
	if u.CompletionDetails != nil {
		if u.CompletionDetails.ReasoningTokens > 0 {
			t["reasoning"] = int64(u.CompletionDetails.ReasoningTokens)
		}
		if u.CompletionDetails.AudioTokens > 0 {
			t["audio_output"] = int64(u.CompletionDetails.AudioTokens)
		}
		if u.CompletionDetails.AcceptedPredictionTokens > 0 {
			t["accepted_prediction"] = int64(u.CompletionDetails.AcceptedPredictionTokens)
		}
		if u.CompletionDetails.RejectedPredictionTokens > 0 {
			t["rejected_prediction"] = int64(u.CompletionDetails.RejectedPredictionTokens)
		}
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// canonicalUsageToCC maps a canonical orthogonal-meter map back to
// CC's Usage block. prompt_tokens is reconstructed as input +
// cache_read (CC's convention). total_tokens is prompt + completion —
// NOT Tokens.Sum(): reasoning/audio are sub-breakdowns already inside
// completion, so summing the whole map double-counts them (OpenAI's own
// total_tokens is just input_tokens + output_tokens).
func canonicalUsageToCC(t usage.Tokens) *Usage {
	if len(t) == 0 {
		return nil
	}
	cached := int(t["cache_read"])
	prompt := int(t["input"]) + cached
	completion := int(t["output"])
	cu := &Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	if cached > 0 {
		cu.PromptDetails = &PromptTokenDetails{CachedTokens: cached}
	}
	if r := int(t["reasoning"]); r > 0 {
		cu.CompletionDetails = &CompletionTokenDetails{ReasoningTokens: r}
	}
	return cu
}
