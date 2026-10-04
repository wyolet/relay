package gemini

import (
	"testing"

	"github.com/wyolet/relay/sdk/internal/fidelity"
)

// Every canonical request field reaches the generateContent wire or is a
// listed, annotated drop (rule 11). A new canonical field fails here until it
// is mapped or annotated.
func TestSerializeRequest_CanonicalFieldFidelity(t *testing.T) {
	body, err := (GeminiTranslator{}).SerializeRequest(fidelity.Request())
	if err != nil {
		t.Fatal(err)
	}
	fidelity.Check(t, body,
		map[string]string{
			"ModelConfig.Output.Format.Type": `"responseMimeType":"application/json"`,
			"Tools.Choice.Mode":              `"mode":"ANY"`,
			"Tools.Choice.FunctionName":      `"allowedFunctionNames":["fidelity_tool"]`,
		},
		map[string]string{
			"Model":                                 "carried in the URL path, not the body",
			"OutputMode":                            "selected by the endpoint (streamGenerateContent), not the body",
			"ModelConfig.Reasoning.Effort":          "thinkingLevel is rejected by pre-3 models",
			"ModelConfig.Reasoning.Summary":         "only includeThoughts, no summary granularity",
			"ModelConfig.Output.Format.Name":        "responseSchema takes the bare schema",
			"ModelConfig.Output.Format.Description": "responseSchema takes the bare schema",
			"ModelConfig.Output.Format.Strict":      "responseSchema takes the bare schema",
			"ModelConfig.Output.Verbosity":          "no verbosity control",
			"Tools.Definitions.Strict":              "function declarations take no strict flag",
			"Tools.Definitions.ProviderData":        "another vendor's tool definition",
			"Tools.Parallel":                        "no parallel function-call switch",
			"CacheConfig.Instructions":              "implicit caching, no breakpoints",
			"CacheConfig.Tools":                     "implicit caching, no breakpoints",
			"CacheConfig.Key":                       "implicit caching, no key",
			"CacheConfig.TTL":                       "implicit caching, no retention knob",
			"User":                                  "no end-user identifier",
			"Metadata":                              "no request metadata",
			"Extensions":                            "no Gemini-owned keys (rule 7)",
		},
	)
}
