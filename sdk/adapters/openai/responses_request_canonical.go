package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ParseRequest decodes a Responses wire body into canonical *v1.Request.
// Rejects stateful OpenAI-isms.
func (ResponsesTranslator) ParseRequest(body []byte) (*v1.Request, error) {
	req, err := ParseResponsesRequest(body)
	if err != nil {
		return nil, fmt.Errorf("responses parse_request: %w", err)
	}

	if err := responsesRejectStatefulFields(req); err != nil {
		return nil, err
	}

	return responsesRequestToCanonical(req)
}

// SerializeRequest encodes a canonical *v1.Request to a Responses wire body.
func (ResponsesTranslator) SerializeRequest(req *v1.Request) ([]byte, error) {
	rreq, err := canonicalToResponsesRequest(req)
	if err != nil {
		return nil, err
	}

	// Serialize Input as a JSON array. Hoist-flagged system items were merged
	// into instructions by canonicalToResponsesRequest — skip them here.
	input, _ := v1.SplitHoistedSystem(req.Input)
	inputRaws := make([]json.RawMessage, 0, len(input))
	for _, item := range input {
		ritem := responsesInputItemFromCanonical(item)
		if ritem == nil {
			continue
		}
		b, err := json.Marshal(ritem)
		if err != nil {
			return nil, fmt.Errorf("responses serialize_request: input item: %w", err)
		}
		inputRaws = append(inputRaws, b)
	}
	inputJSON, err := json.Marshal(inputRaws)
	if err != nil {
		return nil, err
	}

	type wireReq struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`

		Instructions string               `json:"instructions,omitempty"`
		Tools        ResponsesTools       `json:"tools,omitempty"`
		ToolChoice   *ResponsesToolChoice `json:"tool_choice,omitempty"`

		Temperature     *float64 `json:"temperature,omitempty"`
		TopP            *float64 `json:"top_p,omitempty"`
		MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`
		// No top_k / stop_sequences: the Responses API has neither parameter
		// (they exist only on Chat Completions). Emitting them 400s with
		// "Unknown parameter". canonical stop_sequences is dropped at the
		// canonicalToResponsesRequest mapping with a greppable annotation.

		Text      *ResponsesTextConfig      `json:"text,omitempty"`
		Reasoning *ResponsesReasoningConfig `json:"reasoning,omitempty"`

		ParallelToolCalls    *bool             `json:"parallel_tool_calls,omitempty"`
		Metadata             map[string]string `json:"metadata,omitempty"`
		User                 string            `json:"user,omitempty"`
		Stream               *bool             `json:"stream,omitempty"`
		Store                *bool             `json:"store,omitempty"`
		Include              []string          `json:"include,omitempty"`
		PromptCacheKey       string            `json:"prompt_cache_key,omitempty"`
		PromptCacheRetention string            `json:"prompt_cache_retention,omitempty"`
	}
	// Stateless reasoning round-trip: relay's canonical protocol is stateless
	// (it rejects previous_response_id / store / conversation), so the ONLY way
	// a reasoning item can travel with its tool_call across a tool-loop turn —
	// which the Responses API requires by id — is the encrypted reasoning blob.
	// Ask OpenAI to return it (include) and don't persist server-side (store
	// false). The blob round-trips via Reasoning.ProviderData in
	// responsesItemTo/FromCanonical; without the include it's never returned and
	// a function_call comes back without its required reasoning sibling (400).
	storeFalse := false
	return json.Marshal(wireReq{
		Model:                req.Model[0],
		Input:                inputJSON,
		Instructions:         rreq.Instructions,
		Tools:                rreq.Tools,
		ToolChoice:           rreq.ToolChoice,
		Temperature:          rreq.Temperature,
		TopP:                 rreq.TopP,
		MaxOutputTokens:      rreq.MaxOutputTokens,
		Text:                 rreq.Text,
		Reasoning:            rreq.Reasoning,
		ParallelToolCalls:    rreq.ParallelToolCalls,
		Metadata:             req.Metadata,
		User:                 req.User,
		Stream:               rreq.Stream,
		Store:                &storeFalse,
		Include:              []string{includeEncryptedReasoning},
		PromptCacheKey:       rreq.PromptCacheKey,
		PromptCacheRetention: rreq.PromptCacheRetention,
	})
}

// includeEncryptedReasoning is the one include entry that survives a cross-shape round-trip: SerializeRequest asks for it so the reasoning blob comes back, and responsesItemTo/FromCanonical carry it on the item as provider data.
const includeEncryptedReasoning = "reasoning.encrypted_content"

// responsesRejectStatefulFields rejects OpenAI-isms that have no canonical equivalent.
func responsesRejectStatefulFields(req *ResponsesRequest) error {
	if req.PreviousResponseID != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "previous_response_id")
	}
	if req.Store != nil && *req.Store {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "store")
	}
	if req.Conversation != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "conversation")
	}
	if req.Background != nil && *req.Background {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "background")
	}
	if req.Truncation != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "truncation")
	}
	// canonical: service_tier dropped — it asks the upstream account for a latency/price lane, not for different output, and no other vendor has the concept. Codex sends it on every request when the user configures one, so rejecting it would refuse the whole turn over a hint.
	if req.SafetyIdentifier != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "safety_identifier")
	}
	if len(req.ContextManagement) > 0 && string(req.ContextManagement) != "null" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "context_management")
	}
	// include asks the upstream to attach extra payloads to the response. SerializeRequest re-emits includeEncryptedReasoning unconditionally, so that entry survives the round-trip; the rest name payloads the canonical response has no field to carry.
	//
	// canonical: include[<anything but includeEncryptedReasoning>] dropped — an unrequested enrichment degrades the answer, it does not corrupt it, and Codex sends include with no opt-out, so refusing the field would refuse every turn.
	if len(req.Prompt) > 0 && string(req.Prompt) != "null" {
		// A stored prompt template lives in OpenAI's server-side store and carries
		// the actual instructions. Cross-shape we can't resolve it, and silently
		// dropping it would send an empty/wrong request — fail loud, like the other
		// stateful fields. (Byte-pass to OpenAI-native is unaffected.)
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "prompt")
	}
	return nil
}
