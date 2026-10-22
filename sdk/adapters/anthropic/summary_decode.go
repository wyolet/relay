package anthropic

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// DecodeSummary implements v1.SummaryDecoder: ParseResponse's usage, service tier and finish reason, the structured-output tool block's override included, without decoding content.
func (AnthropicTranslator) DecodeSummary(body []byte) (v1.Summary, error) {
	var ar struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name,omitempty"`
		} `json:"content"`
		StopReason string             `json:"stop_reason"`
		Usage      anthropicFullUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return v1.Summary{}, err
	}
	_, finish, _ := anthropicStopReasonToCanonical(ar.StopReason)
	for _, b := range ar.Content {
		if b.Type == "tool_use" && b.Name == structuredOutputToolName {
			finish = v1.FinishReasonStop
		}
	}
	return v1.Summary{Tokens: anthropicUsageToCanonical(&ar.Usage), FinishReason: finish, ServiceTier: ar.Usage.ServiceTier}, nil
}
