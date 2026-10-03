package anthropic

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseResponse ----

// ParseResponse decodes an Anthropic /v1/messages response body into canonical *v1.Response.
func (AnthropicTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	var ar anthropicFullResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("anthropic parse_response: %w", err)
	}

	resp := &v1.Response{
		ID:        ar.ID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     ar.Model,
	}

	// Map stop_reason.
	resp.Status, resp.FinishReason, resp.IncompleteDetails = anthropicStopReasonToCanonical(ar.StopReason)

	// Surface the matched stop_sequence string (rule 7: cross-cutting field that
	// doesn't map cleanly across vendors goes in Extensions).
	if ar.StopSeq != nil && *ar.StopSeq != "" {
		raw, _ := json.Marshal(*ar.StopSeq)
		resp.Extensions = map[string]json.RawMessage{"stop_sequence": raw}
	}

	// Build output items from content blocks.
	outputIndex := 0
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			part := &v1.OutputTextPart{
				Text:        block.Text,
				Annotations: anthropicCitationsToCanonical(block.Citations),
			}
			msg := &v1.Message{
				ID:      fmt.Sprintf("msg_%d", outputIndex),
				Status:  v1.StatusCompleted,
				Role:    v1.RoleAssistant,
				Content: []v1.Part{part},
			}
			resp.Output = append(resp.Output, msg)
			outputIndex++

		case "tool_use":
			if block.Name == structuredOutputToolName {
				// Unwrap the forced-tool trick: emit the tool input as plain text
				// so the caller sees a normal completed text response (rule 9 semantics).
				text := "{}"
				if len(block.Input) > 0 {
					text = string(block.Input)
				}
				msg := &v1.Message{
					ID:      fmt.Sprintf("msg_%d", outputIndex),
					Status:  v1.StatusCompleted,
					Role:    v1.RoleAssistant,
					Content: []v1.Part{&v1.OutputTextPart{Text: text}},
				}
				resp.Output = append(resp.Output, msg)
				resp.Status = v1.StatusCompleted
				resp.FinishReason = v1.FinishReasonStop
				resp.IncompleteDetails = nil
				outputIndex++
				continue
			}
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			fc := &v1.FunctionCall{
				ID:        fmt.Sprintf("fc_%d", outputIndex),
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: args,
				Status:    v1.StatusCompleted,
			}
			resp.Output = append(resp.Output, fc)
			outputIndex++

		case "thinking":
			// Empty-text blocks are NOT skipped: on the 4.7+/Sonnet 5/Fable 5
			// family display defaults to "omitted", so thinking arrives with empty
			// text but a signature that same-model replay must echo back verbatim.
			// Dropping the block loses the signature and the item entirely.
			if block.Thinking == "" && block.Signature == "" {
				continue
			}
			// Carry the full thinking block (including signature) in ProviderData for
			// same-vendor round-trip. Cross-vendor consumers ignore ProviderData.
			var providerData json.RawMessage
			if block.Signature != "" {
				pd := map[string]string{
					"type":      "thinking",
					"thinking":  block.Thinking,
					"signature": block.Signature,
				}
				providerData, _ = json.Marshal(pd)
			}
			r := &v1.Reasoning{
				ID:           fmt.Sprintf("rs_%d", outputIndex),
				Content:      block.Thinking,
				Status:       v1.StatusCompleted,
				ProviderData: providerData,
			}
			if block.Thinking != "" {
				r.Summary = []v1.SummaryText{{Text: block.Thinking}}
			}
			resp.Output = append(resp.Output, r)
			outputIndex++

		case "redacted_thinking":
			// Cannot faithfully represent; silently drop.

		case "server_tool_use":
			// server_tool_use blocks (web_search, code_execution) not modeled in v1 output.

		default:
			// Unknown block types dropped for forward compatibility.
		}
	}

	// Usage: orthogonal-meter map. Each dimension Anthropic prices
	// distinctly (input vs cache_read vs cache_creation) gets its own
	// key. Tokens.Sum() over the map gives the honest "all tokens
	// processed" count without double-counting.
	resp.Usage = anthropicUsageToCanonical(&ar.Usage)

	return resp, nil
}
