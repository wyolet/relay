package anthropic_test

import (
	"encoding/json"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/openai"
)

// A Chat Completions reasoning_effort reaches the Messages wire as
// output_config.effort through the canonical request.
func TestCrossShape_CCReasoningEffortToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-5-5",
		"reasoning_effort": "high",
		"messages": [{"role": "user", "content": "think"}]
	}`)
	req, err := openai.CCTranslator{}.ParseRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	out, err := anthropic.AnthropicTranslator{}.SerializeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
		Thinking struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.OutputConfig.Effort != "high" {
		t.Errorf("output_config.effort: %q, want high (body %s)", wire.OutputConfig.Effort, out)
	}
	if wire.Thinking.Type != "adaptive" {
		t.Errorf("thinking.type: %q, want adaptive (body %s)", wire.Thinking.Type, out)
	}
}
