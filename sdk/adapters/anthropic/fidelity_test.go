package anthropic

import (
	"testing"

	"github.com/wyolet/relay/sdk/internal/fidelity"
)

// Every canonical request field reaches the Messages wire or is a listed,
// annotated drop (rule 11). A new canonical field fails here until it is
// mapped or annotated.
func TestSerializeRequest_CanonicalFieldFidelity(t *testing.T) {
	body, err := (AnthropicTranslator{}).SerializeRequest(fidelity.Request())
	if err != nil {
		t.Fatal(err)
	}
	fidelity.Check(t, body,
		map[string]string{
			"ModelConfig.Reasoning.BudgetTokens": `"budget_tokens":2345`,
			"ModelConfig.Reasoning.Effort":       `"output_config":{"effort":"medium"}`,
			"Tools.Choice.Mode":                  `"type":"tool"`,
			"Tools.Choice.FunctionName":          `"name":"fidelity_tool"`,
			"Tools.Parallel":                     `"disable_parallel_tool_use":true`,
			"CacheConfig.Instructions":           `"system":[{"cache_control":{"ttl":"1h","type":"ephemeral"},"text":"fidelity-instructions"`,
			"CacheConfig.Tools":                  `"fidelity_param":{"type":"string"}}},"cache_control"`,
			"CacheConfig.TTL":                    `"ttl":"1h"`,
			"OutputMode":                         `"stream":true`,
			"User":                               `"user_id":"fidelity-user"`,
		},
		map[string]string{
			"ModelConfig.Sampling.Temperature":      "custom sampling rejected alongside thinking",
			"ModelConfig.Sampling.TopP":             "custom sampling rejected alongside thinking",
			"ModelConfig.Sampling.TopK":             "custom sampling rejected alongside thinking",
			"ModelConfig.Sampling.Seed":             "no seed parameter",
			"ModelConfig.Sampling.FrequencyPenalty": "no frequency penalty",
			"ModelConfig.Sampling.PresencePenalty":  "no presence penalty",
			"ModelConfig.Reasoning.Summary":         "display is set only for adaptive thinking",
			// The fixture forces a tool choice, which wins over structured
			// output; the forced-tool path has its own tests.
			"ModelConfig.Output.Format.Type":        "caller-forced tool choice wins over structured output",
			"ModelConfig.Output.Format.Name":        "fixed structured-output tool name",
			"ModelConfig.Output.Format.Description": "fixed structured-output tool description",
			"ModelConfig.Output.Format.Schema":      "caller-forced tool choice wins over structured output",
			"ModelConfig.Output.Format.Strict":      "tools take no strict flag",
			"ModelConfig.Output.Verbosity":          "no verbosity control",
			"Tools.Definitions.Strict":              "tools take no strict flag",
			"Tools.Definitions.ProviderData":        "another vendor's tool definition",
			"CacheConfig.Key":                       "deterministic breakpoints need no routing key",
			"Metadata":                              "metadata accepts user_id only",
			"Extensions":                            "no Anthropic-owned keys (rule 7)",
		},
	)
}
