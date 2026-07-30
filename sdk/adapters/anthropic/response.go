package anthropic

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseResponse ----

// ParseResponse decodes an Anthropic /v1/messages response body into canonical *v1.Response.
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

// anthropicUsageToCanonical maps Anthropic's response usage block to
// the canonical orthogonal-meter Tokens map. Keys match
// pricing.MeterForUsageKey so the same vocabulary flows from this
// adapter through every downstream observer + pricing computation.
func anthropicUsageToCanonical(u *anthropicFullUsage) usage.Tokens {
	if u == nil {
		return nil
	}
	t := usage.Tokens{}
	if u.InputTokens > 0 {
		t["input"] = int64(u.InputTokens)
	}
	if u.OutputTokens > 0 {
		t["output"] = int64(u.OutputTokens)
	}
	if u.CacheReadInputTokens > 0 {
		t["cache_read"] = int64(u.CacheReadInputTokens)
	}
	if u.CacheCreationInputTokens > 0 {
		t["cache_creation"] = int64(u.CacheCreationInputTokens)
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// ---- SerializeResponse ----

// SerializeResponse encodes a canonical *v1.Response to an Anthropic /v1/messages response body.
// req is unused — Anthropic does not require request echo on the response.
func (AnthropicTranslator) SerializeResponse(resp *v1.Response, _ *v1.Request) ([]byte, error) {
	out := map[string]any{
		"id":    resp.ID,
		"type":  "message",
		"role":  "assistant",
		"model": resp.Model,
	}

	// Map canonical status/finish_reason back to Anthropic stop_reason.
	out["stop_reason"] = canonicalFinishReasonToAnthropic(resp.FinishReason, resp.IncompleteDetails)

	// Build content blocks from output items.
	var content []map[string]any
	for _, item := range resp.Output {
		switch v := item.(type) {
		case *v1.Message:
			for _, p := range v.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					block := map[string]any{
						"type": "text",
						"text": tp.Text,
					}
					if len(tp.Annotations) > 0 {
						block["citations"] = canonicalAnnotationsToAnthropic(tp.Annotations)
					}
					content = append(content, block)
				case *v1.TextPart:
					content = append(content, map[string]any{
						"type": "text",
						"text": tp.Text,
					})
				}
			}
		case *v1.FunctionCall:
			var inputObj any
			if v.Arguments != "" {
				if err := json.Unmarshal([]byte(v.Arguments), &inputObj); err != nil {
					inputObj = map[string]string{"_raw": v.Arguments}
				}
			} else {
				inputObj = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    v.CallID,
				"name":  v.Name,
				"input": inputObj,
			})
		case *v1.Reasoning:
			// Restore from ProviderData if available; otherwise use Content.
			if len(v.ProviderData) > 0 {
				var pd struct {
					Type      string `json:"type"`
					Thinking  string `json:"thinking"`
					Signature string `json:"signature"`
				}
				if err := json.Unmarshal(v.ProviderData, &pd); err == nil && pd.Type == "thinking" {
					block := map[string]any{
						"type":     "thinking",
						"thinking": pd.Thinking,
					}
					if pd.Signature != "" {
						block["signature"] = pd.Signature
					}
					content = append(content, block)
					continue
				}
			}
			if v.Content != "" {
				content = append(content, map[string]any{
					"type":     "thinking",
					"thinking": v.Content,
				})
			} else if len(v.Summary) > 0 {
				content = append(content, map[string]any{
					"type":     "thinking",
					"thinking": v.Summary[0].Text,
				})
			}
		}
	}
	if content == nil {
		content = []map[string]any{}
	}
	out["content"] = content

	// Usage: canonical orthogonal-meter map → Anthropic's named fields.
	if len(resp.Usage) > 0 {
		u := map[string]int64{
			"input_tokens":  resp.Usage["input"],
			"output_tokens": resp.Usage["output"],
		}
		if v := resp.Usage["cache_read"]; v > 0 {
			u["cache_read_input_tokens"] = v
		}
		if v := resp.Usage["cache_creation"]; v > 0 {
			u["cache_creation_input_tokens"] = v
		}
		out["usage"] = u
	}

	return json.Marshal(out)
}

// anthropicStopReasonToCanonical maps an Anthropic stop_reason to canonical status/finish/incomplete.
func anthropicStopReasonToCanonical(reason string) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "max_tokens":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_output_tokens"}
	case "tool_use":
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	case "refusal":
		return v1.StatusCompleted, v1.FinishReasonRefusal, nil
	case "pause_turn":
		return v1.StatusIncomplete, "", &v1.IncompleteDetails{Reason: "pause_turn"}
	default:
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	}
}

// canonicalFinishReasonToAnthropic maps canonical finish_reason + incomplete_details to Anthropic stop_reason string.
func canonicalFinishReasonToAnthropic(reason v1.FinishReason, incomplete *v1.IncompleteDetails) string {
	if incomplete != nil {
		switch incomplete.Reason {
		case "max_output_tokens":
			return "max_tokens"
		case "pause_turn":
			return "pause_turn"
		}
	}
	return canonicalFinishReasonToAnthropicStr(reason)
}

func canonicalFinishReasonToAnthropicStr(reason v1.FinishReason) string {
	switch reason {
	case v1.FinishReasonStop:
		return "end_turn"
	case v1.FinishReasonLength:
		return "max_tokens"
	case v1.FinishReasonToolCalls:
		return "tool_use"
	case v1.FinishReasonRefusal:
		return "refusal"
	case v1.FinishReasonContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicCitationsToCanonical maps Anthropic url_citation annotations to canonical Annotations.
func anthropicCitationsToCanonical(cits []anthropicCitation) []v1.Annotation {
	if len(cits) == 0 {
		return nil
	}
	var out []v1.Annotation
	for _, c := range cits {
		if c.Type == "url_citation" {
			out = append(out, &v1.URLCitationAnnotation{
				URL:        c.URL,
				Title:      c.Title,
				StartIndex: c.StartIndex,
				EndIndex:   c.EndIndex,
			})
		}
		// char_location and page_location dropped — no clean v1 equivalent.
	}
	return out
}

// canonicalAnnotationsToAnthropic maps canonical annotations to Anthropic citations.
func canonicalAnnotationsToAnthropic(anns []v1.Annotation) []map[string]any {
	var out []map[string]any
	for _, a := range anns {
		switch v := a.(type) {
		case *v1.URLCitationAnnotation:
			out = append(out, map[string]any{
				"type":        "url_citation",
				"url":         v.URL,
				"title":       v.Title,
				"start_index": v.StartIndex,
				"end_index":   v.EndIndex,
			})
		}
	}
	return out
}
