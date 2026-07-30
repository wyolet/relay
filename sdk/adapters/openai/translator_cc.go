package openai

import (
	"encoding/json"
)

// CCTranslator implements v1.Translator for the OpenAI Chat Completions wire shape.
// ParseRequest converts a CC /v1/chat/completions body to canonical *v1.Request.
// SerializeRequest converts canonical *v1.Request to a CC body.
// ParseResponse converts a CC non-streaming response to canonical *v1.Response.
// SerializeResponse converts canonical *v1.Response to a CC response body.
// The stream factories handle per-stream CC SSE ↔ canonical SSE translation.
//
// Reasoning content: CC emits reasoning_content in the delta as a non-standard
// field (some o-series upstreams). In canonical, this maps to Reasoning.Content
// (the visible string field), not ProviderData.
//
// Refusal: CC has message.refusal (*string). Canonical rule 9: refusal text lives
// in a normal message item's text content with finish_reason="refusal". On the way
// to CC, finish_reason="refusal" sets message.refusal and nulls out content.
//
// Multiplex: assumes upstream already rejected len(model)>1. SerializeRequest
// takes model[0].
type CCTranslator struct{}

// Reasoning is carried on CC under one of two non-standard delta/message
// fields. OLLAMA DIVERGES FROM OPENAI: it maps its Thinking output to
// "reasoning", whereas OpenAI-compatible o-series / DeepSeek upstreams use
// "reasoning_content". The shared OpenAI adapter handles both rather than
// forking a near-identical pkg/adapters/ollama for a single field name; if
// Ollama's divergence grows beyond this, promote it to its own vendor adapter.
const (
	ccReasoningFieldStd    = "reasoning_content" // o-series / DeepSeek / vLLM
	ccReasoningFieldOllama = "reasoning"         // Ollama
)

// ccReasoningProviderData preserves which CC wire field carried the reasoning
// text. Canonical normalizes both names into the single Reasoning.Content
// ("canonical maps it to one"); this records the original so a canonical→CC
// serialize can echo the field verbatim ("the adapter never invents a field").
type ccReasoningProviderData struct {
	Field string `json:"cc_reasoning_field"`
}

func ccReasoningProviderDataJSON(field string) json.RawMessage {
	if field == "" {
		field = ccReasoningFieldStd
	}
	b, _ := json.Marshal(ccReasoningProviderData{Field: field})
	return b
}

// ccReasoningField reads the preserved wire field from a canonical Reasoning
// item's provider_data, defaulting to reasoning_content.
func ccReasoningField(pd json.RawMessage) string {
	if len(pd) > 0 {
		var d ccReasoningProviderData
		if json.Unmarshal(pd, &d) == nil && d.Field != "" {
			return d.Field
		}
	}
	return ccReasoningFieldStd
}

// ccExtractReasoningContent extracts the reasoning text from a raw CC chunk,
// probing both the OpenAI ("reasoning_content") and Ollama ("reasoning") field
// names. Returns the text and which field carried it (empty when neither).
func ccExtractReasoningContent(raw []byte) (text, field string) {
	var probe struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Choices) == 0 {
		return "", ""
	}
	d := probe.Choices[0].Delta
	if d.ReasoningContent != "" {
		return d.ReasoningContent, ccReasoningFieldStd
	}
	if d.Reasoning != "" {
		return d.Reasoning, ccReasoningFieldOllama
	}
	return "", ""
}
