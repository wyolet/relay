package openai

import (
	"testing"

	"github.com/wyolet/relay/sdk/internal/fidelity"
)

// Every canonical request field reaches the CC wire or is a listed,
// annotated drop (rule 11). A new canonical field fails here until it is
// mapped or annotated.
func TestCCSerializeRequest_CanonicalFieldFidelity(t *testing.T) {
	body, err := (CCTranslator{}).SerializeRequest(fidelity.Request())
	if err != nil {
		t.Fatal(err)
	}
	fidelity.Check(t, body,
		map[string]string{
			"ModelConfig.Reasoning.Effort":     `"reasoning_effort":"medium"`,
			"ModelConfig.Output.Format.Type":   `"type":"json_schema"`,
			"ModelConfig.Output.Format.Strict": `"strict":false`,
			"ModelConfig.Output.Verbosity":     `"verbosity":"low"`,
			"Tools.Definitions.Strict":         `"strict":true`,
			"Tools.Choice.Mode":                `"tool_choice":{"type":"function","function":{"name":"fidelity_tool"}}`,
			"Tools.Choice.FunctionName":        `"tool_choice":{"type":"function","function":{"name":"fidelity_tool"}}`,
			"Tools.Parallel":                   `"parallel_tool_calls":false`,
			"CacheConfig.TTL":                  `"prompt_cache_retention":"24h"`,
			"OutputMode":                       `"stream":true`,
		},
		map[string]string{
			"ModelConfig.Sampling.TopK":          "no top_k parameter",
			"ModelConfig.Reasoning.Summary":      "no reasoning summary parameter",
			"ModelConfig.Reasoning.BudgetTokens": "effort level only, no token budget",
			"Tools.Definitions.ProviderData":     "Responses-only tool definition",
			"CacheConfig.Instructions":           "automatic prefix caching, no breakpoints",
			"CacheConfig.Tools":                  "automatic prefix caching, no breakpoints",
			"Extensions":                         "only openai.* keys are owned (rule 7)",
		},
	)
}

func TestResponsesSerializeRequest_CanonicalFieldFidelity(t *testing.T) {
	body, err := (ResponsesTranslator{}).SerializeRequest(fidelity.Request())
	if err != nil {
		t.Fatal(err)
	}
	fidelity.Check(t, body,
		map[string]string{
			"ModelConfig.Reasoning.Effort":     `"effort":"medium"`,
			"ModelConfig.Reasoning.Summary":    `"summary":"detailed"`,
			"ModelConfig.Output.Format.Type":   `"type":"json_schema"`,
			"ModelConfig.Output.Format.Strict": `"strict":false`,
			"ModelConfig.Output.Verbosity":     `"verbosity":"low"`,
			"Tools.Definitions.Strict":         `"strict":true`,
			"Tools.Choice.Mode":                `"tool_choice":{"type":"function","name":"fidelity_tool"}`,
			"Tools.Choice.FunctionName":        `"tool_choice":{"type":"function","name":"fidelity_tool"}`,
			"Tools.Parallel":                   `"parallel_tool_calls":false`,
			"CacheConfig.TTL":                  `"prompt_cache_retention":"24h"`,
			"OutputMode":                       `"stream":true`,
		},
		map[string]string{
			"ModelConfig.Sampling.TopK":             "no top_k parameter",
			"ModelConfig.Sampling.Stop":             "no stop-sequence parameter",
			"ModelConfig.Sampling.Seed":             "no seed parameter",
			"ModelConfig.Sampling.FrequencyPenalty": "no frequency penalty",
			"ModelConfig.Sampling.PresencePenalty":  "no presence penalty",
			"ModelConfig.Reasoning.BudgetTokens":    "effort level only, no token budget",
			"Tools.Definitions.ProviderData":        "only a custom-tool definition is re-emitted",
			"CacheConfig.Instructions":              "automatic prefix caching, no breakpoints",
			"CacheConfig.Tools":                     "automatic prefix caching, no breakpoints",
			"Extensions":                            "only openai.service_tier is owned here (rule 7)",
		},
	)
}
