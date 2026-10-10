package anthropic

import (
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

func TestServiceTier_ReachesSummary(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"service_tier":"priority"}}`
	s, err := v1.ExtractSummary(AnthropicTranslator{}, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if s.ServiceTier != "priority" {
		t.Fatalf("service tier = %q, want %q", s.ServiceTier, "priority")
	}
}
