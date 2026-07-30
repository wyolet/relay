package gemini

import (
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseResponse ----

// ParseResponse decodes a Gemini generateContent response body into canonical *v1.Response.
func (GeminiTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini parse_response: %w", err)
	}

	resp := &v1.Response{
		ID:        fmt.Sprintf("gemini-%d", time.Now().UnixNano()),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     gr.ModelVersion,
	}

	if len(gr.Candidates) > 0 {
		cand := gr.Candidates[0]

		// Check for function calls in parts before mapping finish reason,
		// because the presence of a functionCall part overrides finishReason.
		hasFunctionCall := false
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					hasFunctionCall = true
					break
				}
			}
		}

		resp.Status, resp.FinishReason, resp.IncompleteDetails = geminiFinishReasonToCanonical(cand.FinishReason, hasFunctionCall)

		if cand.Content != nil {
			outputIndex := 0
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					args := "{}"
					if len(p.FunctionCall.Args) > 0 {
						args = string(p.FunctionCall.Args)
					}
					fc := &v1.FunctionCall{
						ID:        fmt.Sprintf("fc_%d", outputIndex),
						CallID:    geminiCallID(p.FunctionCall.Name, outputIndex), // Gemini has no call ID; synthesize a unique one
						Name:      p.FunctionCall.Name,
						Arguments: args,
						Status:    v1.StatusCompleted,
					}
					if p.ThoughtSignature != "" {
						fc.ProviderData = thoughtSignatureJSON(p.ThoughtSignature)
					}
					resp.Output = append(resp.Output, fc)
					outputIndex++
				} else if p.Text != "" && p.Thought {
					r := &v1.Reasoning{
						ID:      fmt.Sprintf("rs_%d", outputIndex),
						Content: p.Text,
						Summary: []v1.SummaryText{{Text: p.Text}},
						Status:  v1.StatusCompleted,
					}
					if p.ThoughtSignature != "" {
						r.ProviderData = thoughtSignatureJSON(p.ThoughtSignature)
					}
					resp.Output = append(resp.Output, r)
					outputIndex++
				} else if p.Text != "" {
					msg := &v1.Message{
						ID:      fmt.Sprintf("msg_%d", outputIndex),
						Status:  v1.StatusCompleted,
						Role:    v1.RoleAssistant,
						Content: []v1.Part{&v1.OutputTextPart{Text: p.Text}},
					}
					resp.Output = append(resp.Output, msg)
					outputIndex++
				}
			}
		}
	}

	resp.Usage = geminiUsageToTokens(gr.UsageMetadata)

	return resp, nil
}

// ---- SerializeResponse ----

// SerializeResponse encodes a canonical *v1.Response to a Gemini generateContent response body.
// req is unused — Gemini does not require request echo.
func (GeminiTranslator) SerializeResponse(resp *v1.Response, _ *v1.Request) ([]byte, error) {
	var parts []geminiPart
	finishReason := canonicalFinishReasonToGemini(resp.FinishReason, resp.IncompleteDetails)

	for _, item := range resp.Output {
		switch v := item.(type) {
		case *v1.Message:
			for _, p := range v.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					parts = append(parts, geminiPart{Text: tp.Text})
				case *v1.TextPart:
					parts = append(parts, geminiPart{Text: tp.Text})
				}
			}
		case *v1.FunctionCall:
			var argsObj json.RawMessage
			if v.Arguments != "" {
				argsObj = json.RawMessage(v.Arguments)
			} else {
				argsObj = json.RawMessage(`{}`)
			}
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFC{Name: v.Name, Args: argsObj},
			})
		case *v1.Reasoning:
			text := v.Content
			if text == "" && len(v.Summary) > 0 {
				text = v.Summary[0].Text
			}
			if text != "" {
				parts = append(parts, geminiPart{Text: text, Thought: true})
			}
		}
	}

	cand := map[string]any{
		"content":      map[string]any{"role": "model", "parts": parts},
		"finishReason": finishReason,
		"index":        0,
	}

	out := map[string]any{
		"candidates": []any{cand},
	}

	if len(resp.Usage) > 0 {
		um := map[string]int64{}
		if v := resp.Usage["input"]; v > 0 {
			um["promptTokenCount"] = v
		}
		if v := resp.Usage["output"]; v > 0 {
			um["candidatesTokenCount"] = v
		}
		if v := resp.Usage["cache_read"]; v > 0 {
			um["cachedContentTokenCount"] = v
		}
		if v := resp.Usage["reasoning"]; v > 0 {
			um["thoughtsTokenCount"] = v
		}
		out["usageMetadata"] = um
	}

	if resp.Model != "" {
		out["modelVersion"] = resp.Model
	}

	return json.Marshal(out)
}

// geminiFinishReasonToCanonical maps a Gemini finishReason string to canonical status/finish/incomplete.
func geminiFinishReasonToCanonical(reason string, hasFunctionCall bool) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	if hasFunctionCall {
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	}
	switch reason {
	case "STOP", "":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "MAX_TOKENS":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_tokens"}
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		// Content blocked by Gemini's safety/policy filters. Must NOT look like
		// a normal STOP — safety auditing, retry, and billing classification
		// downstream branch on content_filter.
		return v1.StatusIncomplete, v1.FinishReasonContentFilter, &v1.IncompleteDetails{Reason: "content_filter"}
	case "MALFORMED_FUNCTION_CALL":
		// Gemini failed to emit a valid function call — a generation failure,
		// not a clean stop.
		return v1.StatusFailed, v1.FinishReasonStop, &v1.IncompleteDetails{Reason: "malformed_function_call"}
	case "LANGUAGE":
		// Unsupported language — treated as a content filter (output withheld).
		return v1.StatusIncomplete, v1.FinishReasonContentFilter, &v1.IncompleteDetails{Reason: "unsupported_language"}
	default:
		// Unknown/future reason: surface as incomplete with the raw reason
		// rather than silently masquerading as a successful stop.
		return v1.StatusIncomplete, v1.FinishReasonStop, &v1.IncompleteDetails{Reason: "gemini:" + reason}
	}
}

// canonicalFinishReasonToGemini maps canonical finish_reason + incomplete_details back to a Gemini finishReason string.
func canonicalFinishReasonToGemini(reason v1.FinishReason, incomplete *v1.IncompleteDetails) string {
	if incomplete != nil && incomplete.Reason == "max_tokens" {
		return "MAX_TOKENS"
	}
	switch reason {
	case v1.FinishReasonStop:
		return "STOP"
	case v1.FinishReasonLength:
		return "MAX_TOKENS"
	case v1.FinishReasonContentFilter:
		return "SAFETY"
	case v1.FinishReasonToolCalls:
		return "STOP" // Gemini uses STOP even when the last action was a function call
	default:
		return "STOP"
	}
}
