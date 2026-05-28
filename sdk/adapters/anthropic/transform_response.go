// TODO: streaming transform (SSE event-by-event) is a separate follow-up PR.
package anthropic

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/wyolet/relay/sdk/adapters/openai"
)

// ToOpenAIResponse converts a MessagesResponse into an OpenAI ChatResponse.
// stop_reason mapping: end_turn|stop_sequence → "stop", max_tokens → "length",
// tool_use → "tool_calls". Unknown reasons default to "stop".
func ToOpenAIResponse(r *MessagesResponse) (*openai.ChatResponse, error) {
	finish := mapStopReason(r.StopReason)

	var textParts []string
	var toolCalls []openai.ToolCall
	for i, block := range r.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			toolCalls = append(toolCalls, openai.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openai.ToolCallFunction{
					Name:      block.Name,
					Arguments: args,
				},
			})
		default:
			_ = i // ignore thinking/redacted_thinking blocks
		}
	}

	text := strings.Join(textParts, "")
	var contentPtr *string
	if text != "" || len(toolCalls) == 0 {
		contentPtr = &text
	}

	total := r.Usage.InputTokens + r.Usage.OutputTokens
	resp := &openai.ChatResponse{
		ID:      r.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   r.Model,
		Choices: []openai.Choice{
			{
				Index: 0,
				Message: openai.ChatResponseMessage{
					Role:      "assistant",
					Content:   contentPtr,
					ToolCalls: toolCalls,
				},
				FinishReason: finish,
			},
		},
		Usage: &openai.Usage{
			PromptTokens:     r.Usage.InputTokens,
			CompletionTokens: r.Usage.OutputTokens,
			TotalTokens:      total,
		},
	}
	return resp, nil
}

// FromOpenAIResponse converts an OpenAI ChatResponse back into a MessagesResponse.
// Only fields we round-trip are mapped; additional OpenAI fields are dropped.
func FromOpenAIResponse(r *openai.ChatResponse) (*MessagesResponse, error) {
	out := &MessagesResponse{
		ID:    r.ID,
		Type:  "message",
		Role:  "assistant",
		Model: r.Model,
	}

	if len(r.Choices) > 0 {
		ch := r.Choices[0]
		out.StopReason = mapFinishReason(ch.FinishReason)

		msg := ch.Message
		if msg.Content != nil && *msg.Content != "" {
			out.Content = append(out.Content, ContentBlock{
				Type: "text",
				Text: *msg.Content,
			})
		}
		for _, tc := range msg.ToolCalls {
			var input json.RawMessage
			if tc.Function.Arguments != "" {
				input = json.RawMessage(tc.Function.Arguments)
			} else {
				input = json.RawMessage(`{}`)
			}
			out.Content = append(out.Content, ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
	}

	if r.Usage != nil {
		out.Usage = ResponseUsage{
			InputTokens:  r.Usage.PromptTokens,
			OutputTokens: r.Usage.CompletionTokens,
		}
	}

	return out, nil
}

func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// mapFinishReason is the inverse of mapStopReason for round-trip use.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}
