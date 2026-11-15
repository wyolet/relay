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

// Gemini's functionCall.id is optional — without it tool results match calls by function name. Canonical (and OpenAI/Anthropic clients downstream) require a unique CallID per call, so when the id is absent we synthesize CallID = name + callIDSep + index and strip it back to the bare name when emitting a functionResponse upstream. callIDSep is chosen to never collide with a real function name.
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

// geminiWireCallID is the functionCall/functionResponse id to send: empty for a CallID relay derived from the function name, since Gemini never sent one.
func geminiWireCallID(callID, name string) string {
	if callID == name || strings.Contains(callID, callIDSep) {
		return ""
	}
	return callID
}

// thoughtSignatureJSON encodes a thoughtSignature value into the provider_data
// JSON shape used on v1.FunctionCall, v1.Reasoning and v1.Message items.
func thoughtSignatureJSON(sig string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"thoughtSignature": sig})
	return b
}

// emptyPartSignatureJSON is the provider_data of a message whose signature came on a part of its own with empty text; a Message carries no empty text parts, so the flag rebuilds that part.
func emptyPartSignatureJSON(sig string) json.RawMessage {
	b, _ := json.Marshal(struct {
		EmptyTextPart    bool   `json:"emptyTextPart"`
		ThoughtSignature string `json:"thoughtSignature"`
	}{true, sig})
	return b
}

// thoughtSignatureFrom extracts the thoughtSignature from a provider_data blob,
// returning "" if absent or malformed.
func thoughtSignatureFrom(pd json.RawMessage) string {
	sig, _ := itemSignatureFrom(pd)
	return sig
}

// itemSignatureFrom extracts the thoughtSignature and whether it rode a part of its own with empty text.
func itemSignatureFrom(pd json.RawMessage) (sig string, emptyPart bool) {
	if len(pd) == 0 {
		return "", false
	}
	var v struct {
		ThoughtSignature string `json:"thoughtSignature"`
		EmptyTextPart    bool   `json:"emptyTextPart"`
	}
	if err := json.Unmarshal(pd, &v); err != nil {
		return "", false
	}
	return v.ThoughtSignature, v.EmptyTextPart
}

// signLastItem attaches the signature of a non-thought part with empty text to the last of items when that is an unsigned message or reasoning, and reports whether it did. Otherwise the caller keeps the signature on a reasoning item of its own, which other vendors drop.
func signLastItem(items []v1.Item, sig string) bool {
	if len(items) == 0 {
		return false
	}
	switch it := items[len(items)-1].(type) {
	case *v1.Message:
		if len(it.ProviderData) == 0 {
			it.ProviderData = emptyPartSignatureJSON(sig)
			return true
		}
	case *v1.Reasoning:
		if len(it.ProviderData) == 0 {
			it.ProviderData = emptyPartSignatureJSON(sig)
			return true
		}
	}
	return false
}

// reasoningParts renders a reasoning item: its thought part, then its signature where it came.
func reasoningParts(r *v1.Reasoning) []geminiPart {
	text := r.Content
	if text == "" && len(r.Summary) > 0 {
		text = r.Summary[0].Text
	}
	sig, emptyPart := itemSignatureFrom(r.ProviderData)
	var parts []geminiPart
	if text != "" || sig != "" && !emptyPart {
		parts = append(parts, geminiPart{Text: text, Thought: true})
	}
	return signItemParts(parts, 0, r.ProviderData)
}

// signItemParts puts an item's signature back where it came: on its last part (the item's parts are parts[start:]), or on a trailing part with empty text.
func signItemParts(parts []geminiPart, start int, pd json.RawMessage) []geminiPart {
	sig, emptyPart := itemSignatureFrom(pd)
	switch {
	case sig == "":
	case emptyPart || len(parts) == start:
		parts = append(parts, geminiPart{ThoughtSignature: sig})
	default:
		parts[len(parts)-1].ThoughtSignature = sig
	}
	return parts
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
