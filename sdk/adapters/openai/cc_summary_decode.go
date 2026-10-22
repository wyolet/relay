package openai

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage, service tier and finish reason (choices[0]'s, lifted to refusal by its message's refusal), without decoding the messages' content.
func (CCTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	var cc struct {
		ServiceTier string `json:"service_tier,omitempty"`
		Choices     []struct {
			Message struct {
				Refusal *string `json:"refusal,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *Usage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &cc); err != nil {
		return v1.Summary{}, err
	}
	sum := v1.Summary{Tokens: ccUsageToCanonical(cc.Usage), FinishReason: v1.FinishReasonStop, ServiceTier: cc.ServiceTier}
	if len(cc.Choices) > 0 {
		ch := &cc.Choices[0]
		_, sum.FinishReason, _ = ccFinishReasonToCanonical(ch.FinishReason)
		if ch.Message.Refusal != nil && *ch.Message.Refusal != "" && sum.FinishReason == v1.FinishReasonStop {
			sum.FinishReason = v1.FinishReasonRefusal
		}
	}
	return sum, nil
}
