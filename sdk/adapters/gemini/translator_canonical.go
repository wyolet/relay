package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// GeminiTranslator implements v1.Translator for the Gemini generateContent
// wire shape. Stateless value type; per-stream state lives in closures.
type GeminiTranslator struct{}

// Gemini has no per-call IDs — it matches tool results to calls by function
// name. Canonical (and OpenAI/Anthropic clients downstream) require a unique
// CallID per call, so we synthesize CallID = name + callIDSep + index and
// strip it back to the bare name when emitting a functionResponse upstream.
// callIDSep is chosen to never collide with a real function name.
const callIDSep = "__relay_call_"

func geminiCallID(name string, idx int) string {
	return fmt.Sprintf("%s%s%d", name, callIDSep, idx)
}

// geminiFuncNameFromCallID recovers the bare Gemini function name from a
// CallID we synthesized (or returns the input unchanged if it carries no
// suffix — e.g. a CallID minted by a different inbound adapter).
func geminiFuncNameFromCallID(callID string) string {
	if i := strings.LastIndex(callID, callIDSep); i >= 0 {
		return callID[:i]
	}
	return callID
}

// thoughtSignatureJSON encodes a thoughtSignature value into the provider_data
// JSON shape used on v1.FunctionCall and v1.Reasoning items.
func thoughtSignatureJSON(sig string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"thoughtSignature": sig})
	return b
}

// thoughtSignatureFrom extracts the thoughtSignature from a provider_data blob,
// returning "" if absent or malformed.
func thoughtSignatureFrom(pd json.RawMessage) string {
	if len(pd) == 0 {
		return ""
	}
	var v struct {
		ThoughtSignature string `json:"thoughtSignature"`
	}
	if err := json.Unmarshal(pd, &v); err != nil {
		return ""
	}
	return v.ThoughtSignature
}

// resolveModelOpts picks the ModelOpts for the given model name following the
// fallback rules: exact key match → single-entry fallback → nil.
func resolveModelOpts(modelConfig map[string]*v1.ModelOpts, model string) *v1.ModelOpts {
	if o, ok := modelConfig[model]; ok {
		return o
	}
	if len(modelConfig) == 1 {
		for _, o := range modelConfig {
			return o
		}
	}
	return nil
}
