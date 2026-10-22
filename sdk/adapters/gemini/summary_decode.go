package gemini

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage and finish reason (candidates[0]'s, promoted by a functionCall part, or a blocked prompt's), without decoding the parts' content.
func (GeminiTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	var gr struct {
		Candidates []struct {
			Content *struct {
				Parts []struct {
					FunctionCall *struct{} `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content,omitempty"`
			FinishReason string `json:"finishReason,omitempty"`
		} `json:"candidates"`
		UsageMetadata  *usageMetadata  `json:"usageMetadata,omitempty"`
		PromptFeedback json.RawMessage `json:"promptFeedback,omitempty"`
	}
	if err := json.Unmarshal(body, &gr); err != nil {
		return v1.Summary{}, err
	}
	sum := v1.Summary{Tokens: geminiUsageToTokens(gr.UsageMetadata)}
	if len(gr.Candidates) > 0 {
		cand := gr.Candidates[0]
		hasFunctionCall := false
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				hasFunctionCall = hasFunctionCall || p.FunctionCall != nil
			}
		}
		_, sum.FinishReason, _ = geminiFinishReasonToCanonical(cand.FinishReason, hasFunctionCall)
	} else if promptBlockReason(gr.PromptFeedback) != "" {
		_, sum.FinishReason, _ = promptBlocked()
	}
	return sum, nil
}
