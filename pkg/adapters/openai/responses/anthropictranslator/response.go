package anthropictranslator

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/adapters/openai/responses"
)

// anthropicResponseFull is the full Anthropic Messages response shape, including
// fields beyond what pkg/adapters/anthropic.MessagesResponse exposes (cache
// tokens, thinking blocks, citations).
type anthropicResponseFull struct {
	ID         string                `json:"id"`
	Type       string                `json:"type"`
	Role       string                `json:"role"`
	Model      string                `json:"model"`
	Content    []anthropicRespBlock  `json:"content"`
	StopReason string                `json:"stop_reason"`
	StopSeq    *string               `json:"stop_sequence,omitempty"`
	Usage      anthropicUsageFull    `json:"usage"`
}

type anthropicRespBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// thinking block
	Thinking string `json:"thinking,omitempty"`
	// citations — url_citation only; others are v1 lossy
	Citations []anthropicCitation `json:"citations,omitempty"`
}

type anthropicUsageFull struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type anthropicCitation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}

// AnthropicToResponse converts an Anthropic Messages response body (JSON bytes)
// to a Responses API Response.
//
// req is the original Responses API request; its fields are echoed into the
// response per the OpenAI spec. Pass nil for tests that don't need echo fields.
//
// Block mapping:
//   - "text" → message item with output_text part (url_citation annotations mapped;
//     char_location/page_location citations are dropped — v1 lossy)
//   - "tool_use" → function_call item
//   - "thinking" → reasoning item (thinking text → summary)
//   - "redacted_thinking" → silently dropped
//   - "server_tool_use" + any "web_search_tool_result" blocks → dropped
//     (server-side tool calls are not modeled in v1 canonical output)
//
// stop_reason mapping:
//   - "end_turn" | "stop_sequence" → completed / stop
//   - "max_tokens" → incomplete / length
//   - "tool_use" → completed / tool_calls
//   - "refusal" → completed / refusal (text content is the refusal text)
//   - "pause_turn" → incomplete / pause_turn
//   - default → completed / stop
func AnthropicToResponse(req *responses.Request, body []byte) (*responses.Response, error) {
	var ar anthropicResponseFull
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("anthropic response: %w", err)
	}

	resp := &responses.Response{
		ID:        ar.ID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     ar.Model,
	}

	// Build output items from content blocks.
	outputIndex := 0
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			part := &responses.OutputTextPart{
				Text:        block.Text,
				Annotations: mapCitations(block.Citations),
			}
			msg := &responses.Message{
				ID:      fmt.Sprintf("msg_%d", outputIndex),
				Status:  responses.StatusCompleted,
				Role:    responses.RoleAssistant,
				Content: []responses.Part{part},
			}
			resp.Output = append(resp.Output, msg)
			outputIndex++

		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			fc := &responses.FunctionCall{
				ID:        fmt.Sprintf("fc_%d", outputIndex),
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: args,
				Status:    responses.StatusCompleted,
			}
			resp.Output = append(resp.Output, fc)
			outputIndex++

		case "thinking":
			if block.Thinking == "" {
				continue
			}
			r := &responses.Reasoning{
				ID:     fmt.Sprintf("rs_%d", outputIndex),
				Status: responses.StatusCompleted,
				Summary: []responses.SummaryText{
					{Text: block.Thinking},
				},
			}
			resp.Output = append(resp.Output, r)
			outputIndex++

		case "redacted_thinking":
			// Cannot faithfully represent; silently drop.

		case "server_tool_use":
			// Server-side tool calls (e.g. web_search) are not modeled in
			// v1 canonical output. Drop this block and any matching
			// web_search_tool_result blocks (handled below).

		default:
			// Unknown block types are dropped to allow forward compatibility.
		}
	}

	// Map stop_reason → status/finish_reason/incomplete_details.
	status, finish, incomplete := mapStopReason(ar.StopReason)
	resp.Status = status
	resp.FinishReason = finish
	if incomplete != "" {
		resp.IncompleteDetails = &responses.IncompleteDetails{Reason: incomplete}
	}

	// Build usage. input_tokens_details and output_tokens_details are always
	// set (spec required), with zero values when not available.
	total := ar.Usage.InputTokens + ar.Usage.OutputTokens
	u := &responses.Usage{
		InputTokens:         ar.Usage.InputTokens,
		OutputTokens:        ar.Usage.OutputTokens,
		TotalTokens:         total,
		InputTokensDetails:  responses.InputDeets{CachedTokens: ar.Usage.CacheReadInputTokens},
		OutputTokensDetails: responses.OutputDeets{},
	}
	resp.Usage = u

	responses.EchoRequest(resp, req)
	return resp, nil
}

// mapStopReason converts an Anthropic stop_reason to Responses status,
// finish_reason, and incomplete_details.reason.
func mapStopReason(reason string) (responses.Status, responses.FinishReason, string) {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return responses.StatusCompleted, responses.FinishReasonStop, ""
	case "max_tokens":
		return responses.StatusIncomplete, responses.FinishReasonLength, "max_output_tokens"
	case "tool_use":
		return responses.StatusCompleted, responses.FinishReasonToolCalls, ""
	case "refusal":
		return responses.StatusCompleted, "refusal", ""
	case "pause_turn":
		return responses.StatusIncomplete, "", "pause_turn"
	default:
		return responses.StatusCompleted, responses.FinishReasonStop, ""
	}
}

// mapCitations converts Anthropic url_citation annotations to Responses
// URLCitationAnnotation values. char_location and page_location citations
// have no clean Responses equivalent and are dropped (v1 lossy).
func mapCitations(cits []anthropicCitation) []responses.Annotation {
	if len(cits) == 0 {
		return nil
	}
	var out []responses.Annotation
	for _, c := range cits {
		if c.Type == "url_citation" {
			out = append(out, &responses.URLCitationAnnotation{
				URL:        c.URL,
				Title:      c.Title,
				StartIndex: c.StartIndex,
				EndIndex:   c.EndIndex,
			})
		}
		// char_location and page_location citations dropped — no clean
		// Responses equivalent in v1.
	}
	return out
}
