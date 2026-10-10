package anthropic

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

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
	stopReason := canonicalFinishReasonToAnthropic(resp.FinishReason, resp.IncompleteDetails)
	if resp.Status == v1.StatusFailed && resp.FinishReason == "" {
		// canonical: status=failed with no finish_reason must not surface as end_turn
		// (rule 11); refusal is the nearest non-success Anthropic stop_reason.
		stopReason = "refusal"
	}
	out["stop_reason"] = stopReason
	if resp.Error != nil {
		out["error"] = map[string]string{
			"type":    resp.Error.Code,
			"message": resp.Error.Message,
		}
	}

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

	if len(resp.Usage) > 0 {
		out["usage"] = canonicalUsageToAnthropic(resp.Usage)
	}
	// canonical: service_tier dropped — it is the serving upstream's own lane
	// name; writing it into usage.service_tier would mislabel Anthropic's lanes.

	return json.Marshal(out)
}

// canonicalUsageToAnthropic maps the canonical orthogonal-meter map to Anthropic's named usage fields.
func canonicalUsageToAnthropic(t usage.Tokens) map[string]any {
	u := map[string]any{
		"input_tokens":  t["input"],
		"output_tokens": t["output"],
	}
	if v := t["cache_read"]; v > 0 {
		u["cache_read_input_tokens"] = v
	}
	if v := t["cache_creation"]; v > 0 {
		u["cache_creation_input_tokens"] = v
	}
	// The TTL split goes out only when it was reported: without it the
	// share of 5-minute vs 1-hour writes is unknown.
	if h := t["cache_creation_1h"]; h > 0 {
		u["cache_creation"] = map[string]int64{
			"ephemeral_5m_input_tokens": max(t["cache_creation"]-h, 0),
			"ephemeral_1h_input_tokens": h,
		}
	}
	return u
}
