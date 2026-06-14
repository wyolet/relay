package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ResponsesTranslator implements v1.Translator for the OpenAI Responses API wire shape.
// The Responses wire shape is closely aligned with canonical — it is essentially
// "canonical with OpenAI-isms layered on top" (previous_response_id, store, conversation,
// background, etc.). Translation is near-identity for the core fields.
//
// Stateful OpenAI-isms (previous_response_id, store, conversation, background,
// include[], context_management, service_tier, safety_identifier, prompt_cache_key)
// are rejected at ParseRequest with an explicit error so the caller can map to 400.
// These fields have no canonical equivalent in v1.
//
// Request-echo fields required by the OpenAI spec (instructions, temperature, top_p,
// tools, tool_choice, parallel_tool_calls, metadata) are passed explicitly through
// SerializeResponse's req parameter.
type ResponsesTranslator struct{}

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

	// Serialize Input as a JSON array.
	inputRaws := make([]json.RawMessage, 0, len(req.Input))
	for _, item := range req.Input {
		b, err := json.Marshal(responsesItemFromCanonical(item))
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
		TopK            *int     `json:"top_k,omitempty"`
		MaxOutputTokens *int     `json:"max_output_tokens,omitempty"`

		Text      *ResponsesTextConfig      `json:"text,omitempty"`
		Reasoning *ResponsesReasoningConfig `json:"reasoning,omitempty"`

		ParallelToolCalls *bool             `json:"parallel_tool_calls,omitempty"`
		Metadata          map[string]string `json:"metadata,omitempty"`
		User              string            `json:"user,omitempty"`
		Stream            *bool             `json:"stream,omitempty"`
		StopSequences     []string          `json:"stop_sequences,omitempty"`
	}
	return json.Marshal(wireReq{
		Model:             req.Model[0],
		Input:             inputJSON,
		Instructions:      req.Instructions,
		Tools:             rreq.Tools,
		ToolChoice:        rreq.ToolChoice,
		Temperature:       rreq.Temperature,
		TopP:              rreq.TopP,
		TopK:              rreq.TopK,
		MaxOutputTokens:   rreq.MaxOutputTokens,
		Text:              rreq.Text,
		Reasoning:         rreq.Reasoning,
		ParallelToolCalls: rreq.ParallelToolCalls,
		Metadata:          req.Metadata,
		User:              req.User,
	})
}

// ParseResponse decodes a Responses wire response body into canonical *v1.Response.
// Request-echo fields are stripped (they are not part of canonical).
func (ResponsesTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	resp, err := UnmarshalResponsesResponse(body)
	if err != nil {
		return nil, fmt.Errorf("responses parse_response: %w", err)
	}
	return responsesResponseToCanonical(resp), nil
}

// SerializeResponse encodes a canonical *v1.Response to a Responses wire body.
// req is the original canonical request; its fields are echoed into the response
// per the OpenAI spec.
func (ResponsesTranslator) SerializeResponse(resp *v1.Response, req *v1.Request) ([]byte, error) {
	rresp := &ResponsesResponse{
		ID:        resp.ID,
		Object:    "response",
		CreatedAt: resp.CreatedAt,
		Model:     resp.Model,
		Status:    ResponsesStatus(resp.Status),
	}
	if rresp.CreatedAt == 0 {
		rresp.CreatedAt = time.Now().Unix()
	}

	// Map canonical finish_reason to Responses finish_reason.
	rresp.FinishReason = canonicalFinishReasonToResponses(resp.FinishReason)

	// Map output items.
	for _, item := range resp.Output {
		ritem := responsesItemFromCanonical(item)
		if ritem != nil {
			rresp.Output = append(rresp.Output, ritem)
		}
	}

	// Map usage.
	if resp.Usage != nil {
		rresp.Usage = canonicalUsageToResponses(resp.Usage)
	}

	// Map error/incomplete.
	if resp.Error != nil {
		rresp.Error = &ResponsesError{
			Code:    resp.Error.Code,
			Message: resp.Error.Message,
		}
	}
	if resp.IncompleteDetails != nil {
		rresp.IncompleteDetails = &ResponsesIncompleteDetails{
			Reason: resp.IncompleteDetails.Reason,
		}
	}

	// Echo request fields if we have the canonical request.
	if req != nil {
		rreq, err := canonicalToResponsesRequest(req)
		if err == nil {
			ResponsesEchoRequest(rresp, rreq)
		}
	}

	return MarshalResponsesResponse(rresp)
}

// NewToCanonicalStream returns a stateful per-stream function that converts one
// Responses SSE chunk into one or more canonical SSE chunks.
func (ResponsesTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &responsesToCanonicalStream{}
	return s.translate
}

// NewFromCanonicalStream returns a stateful per-stream function that converts
// one canonical SSE chunk into one or more Responses SSE chunks.
func (ResponsesTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToResponsesStream{}
	return s.translate
}

// --- request conversion helpers ---

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
	if req.ServiceTier != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "service_tier")
	}
	if req.SafetyIdentifier != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "safety_identifier")
	}
	if req.PromptCacheKey != "" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "prompt_cache_key")
	}
	if len(req.ContextManagement) > 0 && string(req.ContextManagement) != "null" {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "context_management")
	}
	if len(req.Include) > 0 {
		return fmt.Errorf("responses_unsupported_canonical: field %q has no canonical equivalent", "include")
	}
	return nil
}

// responsesRequestToCanonical maps a *ResponsesRequest to a canonical *v1.Request.
func responsesRequestToCanonical(req *ResponsesRequest) (*v1.Request, error) {
	cr := &v1.Request{
		Model:        v1.ModelRefs{req.Model},
		Instructions: req.Instructions,
		User:         req.User,
		Metadata:     req.Metadata,
	}

	if req.Stream != nil && *req.Stream {
		cr.OutputMode = v1.OutputModeStream
	} else {
		cr.OutputMode = v1.OutputModeSync
	}

	// Build canonical input from Responses items.
	input := make([]v1.Item, 0, len(req.Input))
	for _, item := range req.Input {
		ci, err := responsesItemToCanonical(item)
		if err != nil {
			return nil, fmt.Errorf("input item: %w", err)
		}
		if ci != nil {
			input = append(input, ci)
		}
	}
	cr.Input = input

	// Build ModelOpts.
	opts := &v1.ModelOpts{}
	hasOpts := false

	// Sampling params.
	if req.Temperature != nil || req.TopP != nil || req.MaxOutputTokens != nil || req.TopK != nil ||
		len(req.StopSequences) > 0 {
		sp := &v1.SamplingParams{}
		sp.Temperature = req.Temperature
		sp.TopP = req.TopP
		if req.MaxOutputTokens != nil {
			sp.MaxTokens = req.MaxOutputTokens
		}
		sp.Stop = req.StopSequences
		opts.Sampling = sp
		hasOpts = true
	}

	// Tools.
	if len(req.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, t := range req.Tools {
			ft, ok := t.(*ResponsesFunctionTool)
			if !ok {
				return nil, fmt.Errorf("unsupported tool type %T in Responses request", t)
			}
			params := ft.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			tc.Definitions = append(tc.Definitions, &v1.FunctionTool{
				Name:        ft.Name,
				Description: ft.Description,
				Parameters:  params,
				Strict:      ft.Strict,
			})
		}
		tc.Parallel = req.ParallelToolCalls
		if req.ToolChoice != nil {
			choice := &v1.ToolChoice{
				Mode:         req.ToolChoice.Mode,
				FunctionName: req.ToolChoice.FunctionName,
			}
			tc.Choice = choice
		}
		cr.Tools = tc
	}

	// Reasoning.
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		opts.Reasoning = &v1.ReasoningConfig{Effort: req.Reasoning.Effort}
		hasOpts = true
	}

	// Output format.
	if req.Text != nil && req.Text.Format != nil {
		oc := &v1.OutputConfig{
			Format: &v1.Format{
				Type:   req.Text.Format.Type,
				Name:   req.Text.Format.Name,
				Schema: req.Text.Format.Schema,
				Strict: req.Text.Format.Strict,
			},
		}
		opts.Output = oc
		hasOpts = true
	}

	if hasOpts {
		cr.ModelConfig = map[string]*v1.ModelOpts{req.Model: opts}
	}

	return cr, nil
}

// canonicalToResponsesRequest maps a canonical *v1.Request back to a *ResponsesRequest.
// Used for SerializeRequest and for echo fields in SerializeResponse.
// canonicalToResponsesRequest maps a canonical *v1.Request back to a *ResponsesRequest.
// Used for SerializeRequest and for echo fields in SerializeResponse.
func canonicalToResponsesRequest(req *v1.Request) (*ResponsesRequest, error) {
	if len(req.Model) == 0 {
		return nil, fmt.Errorf("canonical request has no model")
	}
	model := req.Model[0]

	rreq := &ResponsesRequest{
		Model:        model,
		Instructions: req.Instructions,
		User:         req.User,
		Metadata:     req.Metadata,
	}

	if req.OutputMode == v1.OutputModeStream {
		t := true
		rreq.Stream = &t
	}

	opts := req.ModelConfig[model]
	if opts != nil {
		if opts.Sampling != nil {
			s := opts.Sampling
			rreq.Temperature = s.Temperature
			rreq.TopP = s.TopP
			rreq.MaxOutputTokens = s.MaxTokens
			rreq.StopSequences = s.Stop
			// canonical: Seed has no Responses wire equivalent — dropped
			// canonical: FrequencyPenalty has no Responses wire equivalent — dropped
			// canonical: PresencePenalty has no Responses wire equivalent — dropped
			// TopK not in v1 canonical sampling params — omit
		}
		if opts.Reasoning != nil {
			rc := &ResponsesReasoningConfig{Effort: opts.Reasoning.Effort}
			// R-5: map canonical Summary to Responses reasoning.summary field.
			if opts.Reasoning.Summary != "" {
				rc.Summary = opts.Reasoning.Summary
			}
			// canonical: BudgetTokens has no Responses wire equivalent — dropped
			rreq.Reasoning = rc
		}
		if opts.Output != nil && opts.Output.Format != nil {
			rreq.Text = &ResponsesTextConfig{Format: &ResponsesFormat{
				Type:   opts.Output.Format.Type,
				Name:   opts.Output.Format.Name,
				Schema: opts.Output.Format.Schema,
				Strict: opts.Output.Format.Strict,
			}}
		}
	}

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
		for _, tool := range tc.Definitions {
			ft, ok := tool.(*v1.FunctionTool)
			if !ok {
				continue
			}
			params := ft.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			rreq.Tools = append(rreq.Tools, &ResponsesFunctionTool{
				Name:        ft.Name,
				Description: ft.Description,
				Parameters:  params,
				Strict:      ft.Strict,
			})
		}
		rreq.ParallelToolCalls = tc.Parallel
		if tc.Choice != nil {
			rreq.ToolChoice = &ResponsesToolChoice{
				Mode:         tc.Choice.Mode,
				FunctionName: tc.Choice.FunctionName,
			}
		}
	}

	return rreq, nil
}

// responsesItemToCanonical converts a ResponsesItem to a canonical v1.Item.
// responsesItemToCanonical converts a ResponsesItem to a canonical v1.Item.
func responsesItemToCanonical(item ResponsesItem) (v1.Item, error) {
	switch v := item.(type) {
	case *ResponsesMessage:
		parts := make([]v1.Part, 0, len(v.Content))
		for _, p := range v.Content {
			cp, err := responsesPartToCanonical(p)
			if err != nil {
				return nil, err
			}
			if cp != nil {
				parts = append(parts, cp)
			}
		}
		return &v1.Message{
			ID:      v.ID,
			Status:  v1.Status(v.Status),
			Role:    v1.Role(v.Role),
			Content: parts,
		}, nil

	case *ResponsesFunctionCall:
		return &v1.FunctionCall{
			ID:        v.ID,
			CallID:    v.CallID,
			Name:      v.Name,
			Arguments: v.Arguments,
			Status:    v1.Status(v.Status),
		}, nil

	case *ResponsesFunctionCallOutput:
		out := &v1.FunctionCallOutput{CallID: v.CallID, Output: v.Output}
		for _, p := range v.Content {
			cp, err := responsesPartToCanonical(p)
			if err != nil {
				return nil, err
			}
			if cp != nil {
				out.Content = append(out.Content, cp)
			}
		}
		return out, nil

	case *ResponsesReasoning:
		r := &v1.Reasoning{
			ID:     v.ID,
			Status: v1.Status(v.Status),
		}
		for _, s := range v.Summary {
			r.Summary = append(r.Summary, v1.SummaryText{Text: s.Text})
		}
		// R-1: store encrypted_content + item id in ProviderData for same-vendor round-trip.
		if v.EncryptedContent != "" {
			type reasoningProviderData struct {
				EncryptedContent string `json:"encrypted_content"`
				ID               string `json:"id,omitempty"`
			}
			if b, err := json.Marshal(reasoningProviderData{
				EncryptedContent: v.EncryptedContent,
				ID:               v.ID,
			}); err == nil {
				r.ProviderData = b
			}
		}
		return r, nil

	default:
		return nil, fmt.Errorf("unsupported item type %T", item)
	}
}

// responsesPartToCanonical converts a ResponsesPart to a canonical v1.Part.
// RefusalPart → OutputTextPart (canonical rule 9: refusal is text + finish_reason).
func responsesPartToCanonical(p ResponsesPart) (v1.Part, error) {
	switch v := p.(type) {
	case *ResponsesTextPart:
		return &v1.TextPart{Text: v.Text}, nil
	case *ResponsesOutputTextPart:
		out := &v1.OutputTextPart{Text: v.Text}
		for _, a := range v.Annotations {
			ca := responsesAnnotationToCanonical(a)
			if ca != nil {
				out.Annotations = append(out.Annotations, ca)
			}
		}
		return out, nil
	case *ResponsesImagePart:
		return &v1.ImagePart{ImageURL: v.ImageURL, Detail: v.Detail}, nil
	case *ResponsesFilePart:
		return &v1.FilePart{
			FileURL:  v.FileURL,
			FileID:   v.FileID,
			FileData: v.FileData,
			Filename: v.Filename,
		}, nil
	case *ResponsesRefusalPart:
		// Canonical rule 9: refusal text lives in normal message content.
		// Map refusal part → OutputTextPart carrying the refusal text.
		return &v1.OutputTextPart{Text: v.Refusal}, nil
	default:
		return nil, fmt.Errorf("unsupported part type %T", p)
	}
}

// responsesAnnotationToCanonical converts a ResponsesAnnotation to a canonical v1.Annotation.
// responsesAnnotationToCanonical converts a ResponsesAnnotation to a canonical v1.Annotation.
// R-4: file_citation is preserved as *v1.RawAnnotation for forward compatibility.
func responsesAnnotationToCanonical(a ResponsesAnnotation) v1.Annotation {
	switch v := a.(type) {
	case *ResponsesURLCitationAnnotation:
		return &v1.URLCitationAnnotation{
			StartIndex: v.StartIndex,
			EndIndex:   v.EndIndex,
			URL:        v.URL,
			Title:      v.Title,
		}
	case *ResponsesFileCitationAnnotation:
		// file_citation has no dedicated canonical field; preserve as RawAnnotation
		// so it survives same-vendor round-trips without data loss.
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		return &v1.RawAnnotation{Type: "file_citation", JSON: b}
	default:
		return nil
	}
}

// responsesItemFromCanonical converts a canonical v1.Item to a ResponsesItem.
// responsesItemFromCanonical converts a canonical v1.Item to a ResponsesItem.
func responsesItemFromCanonical(item v1.Item) ResponsesItem {
	switch v := item.(type) {
	case *v1.Message:
		parts := make([]ResponsesPart, 0, len(v.Content))
		for _, p := range v.Content {
			rp := responsesPartFromCanonical(p)
			if rp != nil {
				parts = append(parts, rp)
			}
		}
		return &ResponsesMessage{
			ID:      v.ID,
			Status:  ResponsesStatus(v.Status),
			Role:    ResponsesRole(v.Role),
			Content: parts,
		}

	case *v1.FunctionCall:
		return &ResponsesFunctionCall{
			ID:        v.ID,
			CallID:    v.CallID,
			Name:      v.Name,
			Arguments: v.Arguments,
			Status:    ResponsesStatus(v.Status),
		}

	case *v1.FunctionCallOutput:
		out := &ResponsesFunctionCallOutput{
			CallID: v.CallID,
			Output: v.Output,
		}
		for _, p := range v.Content {
			rp := responsesPartFromCanonical(p)
			if rp != nil {
				out.Content = append(out.Content, rp)
			}
		}
		return out

	case *v1.Reasoning:
		r := &ResponsesReasoning{
			ID:     v.ID,
			Status: ResponsesStatus(v.Status),
		}
		for _, s := range v.Summary {
			r.Summary = append(r.Summary, ResponsesSummaryText{Text: s.Text})
		}
		// R-1: restore encrypted_content from ProviderData for same-vendor round-trip.
		if len(v.ProviderData) > 0 {
			var pd struct {
				EncryptedContent string `json:"encrypted_content"`
			}
			if json.Unmarshal(v.ProviderData, &pd) == nil {
				r.EncryptedContent = pd.EncryptedContent
			}
		}
		return r

	default:
		return nil
	}
}

// responsesPartFromCanonical converts a canonical v1.Part to a ResponsesPart.
func responsesPartFromCanonical(p v1.Part) ResponsesPart {
	switch v := p.(type) {
	case *v1.TextPart:
		return &ResponsesTextPart{Text: v.Text}
	case *v1.OutputTextPart:
		out := &ResponsesOutputTextPart{Text: v.Text}
		for _, a := range v.Annotations {
			ra := responsesAnnotationFromCanonical(a)
			if ra != nil {
				out.Annotations = append(out.Annotations, ra)
			}
		}
		return out
	case *v1.ImagePart:
		return &ResponsesImagePart{ImageURL: v.ImageURL, Detail: v.Detail}
	case *v1.FilePart:
		return &ResponsesFilePart{
			FileURL:  v.FileURL,
			FileID:   v.FileID,
			FileData: v.FileData,
			Filename: v.Filename,
		}
	default:
		return nil
	}
}

// responsesAnnotationFromCanonical converts a canonical v1.Annotation to a ResponsesAnnotation.
// responsesAnnotationFromCanonical converts a canonical v1.Annotation to a ResponsesAnnotation.
func responsesAnnotationFromCanonical(a v1.Annotation) ResponsesAnnotation {
	switch v := a.(type) {
	case *v1.URLCitationAnnotation:
		return &ResponsesURLCitationAnnotation{
			StartIndex: v.StartIndex,
			EndIndex:   v.EndIndex,
			URL:        v.URL,
			Title:      v.Title,
		}
	case *v1.RawAnnotation:
		// Round-trip opaque annotation types (e.g. file_citation) verbatim.
		if v.Type == "file_citation" && len(v.JSON) > 0 {
			var fc ResponsesFileCitationAnnotation
			if json.Unmarshal(v.JSON, &fc) == nil {
				return &fc
			}
		}
		return &ResponsesRawAnnotation{Type: v.Type, JSON: v.JSON}
	default:
		return nil
	}
}

// responsesResponseToCanonical converts a *ResponsesResponse to canonical *v1.Response.
func responsesResponseToCanonical(resp *ResponsesResponse) *v1.Response {
	cr := &v1.Response{
		ID:        resp.ID,
		Object:    "response",
		CreatedAt: resp.CreatedAt,
		Model:     resp.Model,
		Status:    v1.Status(resp.Status),
	}
	cr.FinishReason = responsesFinishReasonToCanonical(resp.FinishReason)

	for _, item := range resp.Output {
		ci, _ := responsesItemToCanonical(item)
		if ci != nil {
			cr.Output = append(cr.Output, ci)
		}
	}

	if resp.Usage != nil {
		cr.Usage = responsesUsageToCanonical(resp.Usage)
	}
	if resp.Error != nil {
		cr.Error = &v1.Error{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	if resp.IncompleteDetails != nil {
		cr.IncompleteDetails = &v1.IncompleteDetails{Reason: resp.IncompleteDetails.Reason}
	}

	return cr
}

// responsesFinishReasonToCanonical maps a Responses finish_reason to canonical.
func responsesFinishReasonToCanonical(fr ResponsesFinishReason) v1.FinishReason {
	switch fr {
	case ResponsesFinishReasonStop:
		return v1.FinishReasonStop
	case ResponsesFinishReasonLength:
		return v1.FinishReasonLength
	case ResponsesFinishReasonToolCalls:
		return v1.FinishReasonToolCalls
	case ResponsesFinishReasonContentFilter:
		return v1.FinishReasonContentFilter
	default:
		return v1.FinishReasonStop
	}
}

// canonicalFinishReasonToResponses maps a canonical finish_reason to Responses.
func canonicalFinishReasonToResponses(fr v1.FinishReason) ResponsesFinishReason {
	switch fr {
	case v1.FinishReasonStop:
		return ResponsesFinishReasonStop
	case v1.FinishReasonLength:
		return ResponsesFinishReasonLength
	case v1.FinishReasonToolCalls:
		return ResponsesFinishReasonToolCalls
	case v1.FinishReasonContentFilter:
		return ResponsesFinishReasonContentFilter
	case v1.FinishReasonRefusal:
		// Refusal: canonical has a dedicated finish_reason; Responses doesn't.
		// Map to stop (the refusal text is in the message content).
		return ResponsesFinishReasonStop
	default:
		return ResponsesFinishReasonStop
	}
}

// responsesUsageToCanonical maps Responses' Usage block to the
// canonical orthogonal-meter Tokens map. Same semantics as
// ccUsageToCanonical — Responses' input_tokens INCLUDES cached, so
// we subtract cached out to keep dimensions non-overlapping.
func responsesUsageToCanonical(u *ResponsesUsage) usage.Tokens {
	if u == nil {
		return nil
	}
	t := usage.Tokens{}
	cached := int64(u.InputTokensDetails.CachedTokens)
	if v := int64(u.InputTokens) - cached; v > 0 {
		t["input"] = v
	}
	if u.OutputTokens > 0 {
		t["output"] = int64(u.OutputTokens)
	}
	if cached > 0 {
		t["cache_read"] = cached
	}
	if u.OutputTokensDetails.ReasoningTokens > 0 {
		t["reasoning"] = int64(u.OutputTokensDetails.ReasoningTokens)
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// canonicalUsageToResponses maps a canonical orthogonal-meter map to
// ResponsesUsage. Mirrors canonicalUsageToCC but in Responses shape.
func canonicalUsageToResponses(t usage.Tokens) *ResponsesUsage {
	if len(t) == 0 {
		return nil
	}
	cached := int(t["cache_read"])
	input := int(t["input"]) + cached
	output := int(t["output"])
	r := &ResponsesUsage{
		InputTokens:        input,
		OutputTokens:       output,
		TotalTokens:        int(t.Sum()),
		InputTokensDetails: ResponsesInputDeets{CachedTokens: cached},
	}
	if reasoning := int(t["reasoning"]); reasoning > 0 {
		r.OutputTokensDetails = ResponsesOutputDeets{ReasoningTokens: reasoning}
	}
	return r
}

// --- Responses → canonical stream ---

// responsesToCanonicalStream converts Responses SSE events to canonical SSE frames.
// The Responses stream already uses item-based events closely aligned with canonical.
type responsesToCanonicalStream struct {
	responseID string
	model      string
	created    int64
	started    bool
}

func (s *responsesToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := ParseResponsesSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	var frames []v1.SSEFrame

	switch event {
	case ResponsesEventCreated:
		var ev ResponsesCreatedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, fmt.Errorf("responses stream: created event: %w", err)
		}
		if ev.Response != nil {
			s.responseID = ev.Response.ID
			s.model = ev.Response.Model
			s.created = ev.Response.CreatedAt
		}
		if !s.started {
			s.started = true
			createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
				ID:    s.responseID,
				Model: s.model,
			})
			frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCreated, Data: createdData})
		}

	case ResponsesEventInProgress:
		// No canonical equivalent; ignore.

	case ResponsesEventOutputItemAdded:
		// Two-phase parse: extract output_index and the item's id+type from
		// flat fields (Item is a ResponsesItem interface; json.Unmarshal cannot
		// populate it without a custom dispatcher). Use the item's "type" field
		// directly to determine canonical ItemType.
		var evHeader struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data, &evHeader); err != nil {
			return nil, nil
		}
		if len(evHeader.Item) == 0 || string(evHeader.Item) == "null" {
			return nil, nil
		}
		var itemProbe struct {
			Type ResponsesItemType `json:"type"`
			ID   string            `json:"id"`
		}
		if err := json.Unmarshal(evHeader.Item, &itemProbe); err != nil {
			return nil, nil
		}
		if itemProbe.ID == "" || itemProbe.Type == "" {
			return nil, nil
		}
		startData, _ := json.Marshal(v1.ItemStartedEvent{
			ItemID:   itemProbe.ID,
			ItemType: v1.ItemType(itemProbe.Type),
			Index:    evHeader.OutputIndex,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemStarted, Data: startData})

	case ResponsesEventOutputTextDelta:
		var ev ResponsesOutputTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindText,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	case ResponsesEventFunctionCallArgumentsDelta:
		var ev ResponsesFunctionCallArgumentsDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindArguments,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	case ResponsesEventReasoningTextDelta:
		var ev ResponsesReasoningTextDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindReasoning,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	// R-2: refusal deltas map to text deltas (canonical rule 9: refusal is text +
	// finish_reason, not a separate item type).
	case ResponsesEventRefusalDelta:
		var ev ResponsesRefusalDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
			ItemID: ev.ItemID,
			Index:  ev.OutputIndex,
			Kind:   v1.DeltaKindText,
			Delta:  ev.Delta,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemDelta, Data: deltaData})

	case ResponsesEventRefusalDone:
		// The done event carries no new content — the refusal text was streamed
		// via refusal.delta events above. The terminal finish_reason=refusal is
		// emitted by the response.completed/incomplete handler below.

	case ResponsesEventOutputItemDone:
		// Two-phase parse: extract output_index and the raw item bytes.
		// ResponsesOutputItemDoneEvent.Item is a ResponsesItem interface that
		// json.Unmarshal cannot populate — unmarshal the item bytes separately
		// via responsesUnmarshalItem which uses the "type" discriminator.
		var evHeader struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal(data, &evHeader); err != nil {
			return nil, nil
		}
		if len(evHeader.Item) == 0 || string(evHeader.Item) == "null" {
			return nil, nil
		}
		wireItem, err := responsesUnmarshalItem(evHeader.Item)
		if err != nil {
			return nil, nil
		}
		ci, _ := responsesItemToCanonical(wireItem)
		if ci == nil {
			return nil, nil
		}
		completedData, _ := json.Marshal(v1.ItemCompletedEvent{
			ItemID: responsesItemID(wireItem),
			Index:  evHeader.OutputIndex,
			Item:   ci,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventItemCompleted, Data: completedData})

	case ResponsesEventCompleted, ResponsesEventIncomplete:
		var ev ResponsesCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		if ev.Response == nil {
			return nil, nil
		}
		cr := responsesResponseToCanonical(ev.Response)
		completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
			ID:           cr.ID,
			Status:       cr.Status,
			FinishReason: cr.FinishReason,
			Usage:        cr.Usage,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	// R-2: response.failed means the generation terminated with an error; emit
	// generation.completed with StatusFailed so the consumer isn't left hanging.
	case ResponsesEventFailed:
		var ev ResponsesFailedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		if ev.Response == nil {
			errData, _ := json.Marshal(v1.ErrorEvent{Code: "response_failed", Message: "response failed"})
			frames = append(frames, v1.SSEFrame{Event: v1.EventError, Data: errData})
			return marshalCanonicalFrames(frames), nil
		}
		cr := responsesResponseToCanonical(ev.Response)
		completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
			ID:           cr.ID,
			Status:       v1.StatusFailed,
			FinishReason: cr.FinishReason,
			Usage:        cr.Usage,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	case ResponsesEventError:
		var ev ResponsesErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		errData, _ := json.Marshal(v1.ErrorEvent{
			Code:    ev.Code,
			Message: ev.Message,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventError, Data: errData})

	default:
		// Unknown/unhandled events (content_part.added, output_text.done, etc.) are dropped.
		// They carry no information not already covered by the 6 canonical events.
	}

	return marshalCanonicalFrames(frames), nil
}

// responsesItemID extracts the ID field from a ResponsesItem via type assertion.
func responsesItemID(item ResponsesItem) string {
	switch v := item.(type) {
	case *ResponsesMessage:
		return v.ID
	case *ResponsesFunctionCall:
		return v.ID
	case *ResponsesReasoning:
		return v.ID
	default:
		return ""
	}
}

// --- canonical → Responses stream ---

// canonicalToResponsesStream converts canonical SSE frames to Responses SSE frames.
// This is the "from canonical" direction: used when serving Responses inbound callers
// whose upstream was translated through canonical.
type canonicalToResponsesStream struct {
	responseID    string
	model         string
	created       int64
	outputItems   map[string]responsesStreamItem // itemID → state
	outputIndex   map[string]int                 // itemID → outputIndex
	closedItems   []ResponsesItem
	lifecycleDone bool
}

type responsesStreamItem struct {
	itemType    v1.ItemType
	outputIndex int
	textBuf     string
	argsBuf     string
	callID      string
	name        string
}

func (s *canonicalToResponsesStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	if s.outputItems == nil {
		s.outputItems = make(map[string]responsesStreamItem)
		s.outputIndex = make(map[string]int)
	}

	var frames []ResponsesSSEFrame

	switch event {
	case v1.EventGenerationCreated:
		var ev v1.GenerationCreatedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		s.responseID = ev.ID
		s.model = ev.Model
		s.created = time.Now().Unix()

		stub := &ResponsesResponse{
			ID:        s.responseID,
			Object:    "response",
			CreatedAt: s.created,
			Model:     s.model,
			Status:    ResponsesStatusInProgress,
			Output:    []ResponsesItem{},
		}
		createdData, _ := json.Marshal(ResponsesCreatedEvent{Response: stub})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventCreated, Data: createdData})
		inProgData, _ := json.Marshal(ResponsesInProgressEvent{Response: stub})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventInProgress, Data: inProgData})

	case v1.EventItemStarted:
		var ev v1.ItemStartedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		// R-3: capture name from item.started so function call events carry it.
		// Use itemID as provisional callID — the real callID arrives on item.completed.
		s.outputItems[ev.ItemID] = responsesStreamItem{
			itemType:    ev.ItemType,
			outputIndex: ev.Index,
			name:        ev.Name,
			callID:      ev.ItemID, // provisional; overwritten from item.completed payload
		}
		s.outputIndex[ev.ItemID] = ev.Index
		switch ev.ItemType {
		case v1.ItemTypeMessage:
			msgItem := &ResponsesMessage{
				ID:     ev.ItemID,
				Role:   ResponsesRoleAssistant,
				Status: ResponsesStatusInProgress,
			}
			addedData, _ := json.Marshal(ResponsesItemAddedEvent{OutputIndex: ev.Index, Item: msgItem})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemAdded, Data: addedData})
			partData, _ := json.Marshal(ResponsesContentPartAddedEvent{
				ItemID:       ev.ItemID,
				OutputIndex:  ev.Index,
				ContentIndex: 0,
				Part:         &ResponsesOutputTextPart{Text: ""},
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventContentPartAdded, Data: partData})

		case v1.ItemTypeFunctionCall:
			fcItem := &ResponsesFunctionCall{
				ID:     ev.ItemID,
				Name:   ev.Name,
				Status: ResponsesStatusInProgress,
			}
			addedData, _ := json.Marshal(ResponsesItemAddedEvent{OutputIndex: ev.Index, Item: fcItem})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemAdded, Data: addedData})

		case v1.ItemTypeReasoning:
			rItem := &ResponsesReasoning{
				ID:     ev.ItemID,
				Status: ResponsesStatusInProgress,
			}
			addedData, _ := json.Marshal(ResponsesItemAddedEvent{OutputIndex: ev.Index, Item: rItem})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemAdded, Data: addedData})
		}

	case v1.EventItemDelta:
		var ev v1.ItemDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		st, ok := s.outputItems[ev.ItemID]
		if !ok {
			return nil, nil
		}
		switch ev.Kind {
		case v1.DeltaKindText:
			st.textBuf += ev.Delta
			s.outputItems[ev.ItemID] = st
			deltaData, _ := json.Marshal(ResponsesOutputTextDeltaEvent{
				ItemID:       ev.ItemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Delta:        ev.Delta,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputTextDelta, Data: deltaData})

		case v1.DeltaKindArguments:
			st.argsBuf += ev.Delta
			s.outputItems[ev.ItemID] = st
			// R-3: emit callID and name from stored per-item state.
			deltaData, _ := json.Marshal(ResponsesFunctionCallArgumentsDeltaEvent{
				ItemID:      ev.ItemID,
				OutputIndex: st.outputIndex,
				CallID:      st.callID,
				Delta:       ev.Delta,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventFunctionCallArgumentsDelta, Data: deltaData})

		case v1.DeltaKindReasoning:
			st.textBuf += ev.Delta
			s.outputItems[ev.ItemID] = st
			deltaData, _ := json.Marshal(ResponsesReasoningTextDeltaEvent{
				ItemID:       ev.ItemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Delta:        ev.Delta,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventReasoningTextDelta, Data: deltaData})
		}

	case v1.EventItemCompleted:
		// Two-phase parse: extract item_id and index from a flat struct first
		// (the Item field is a v1.Item interface that json.Unmarshal cannot
		// populate without a custom dispatcher — the full item is not needed
		// because per-stream state in s.outputItems already holds type + buffers).
		var evHeader struct {
			ItemID string `json:"item_id"`
			Index  int    `json:"index"`
		}
		if err := json.Unmarshal(data, &evHeader); err != nil {
			return nil, nil
		}
		st, ok := s.outputItems[evHeader.ItemID]
		if !ok {
			return nil, nil
		}
		itemID := evHeader.ItemID

		switch st.itemType {
		case v1.ItemTypeMessage:
			finalPart := &ResponsesOutputTextPart{Text: st.textBuf}
			textDoneData, _ := json.Marshal(ResponsesOutputTextDoneEvent{
				ItemID:       itemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Text:         st.textBuf,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputTextDone, Data: textDoneData})
			partDoneData, _ := json.Marshal(ResponsesContentPartDoneEvent{
				ItemID:       itemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Part:         finalPart,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventContentPartDone, Data: partDoneData})
			finalMsg := &ResponsesMessage{
				ID:      itemID,
				Role:    ResponsesRoleAssistant,
				Status:  ResponsesStatusCompleted,
				Content: []ResponsesPart{finalPart},
			}
			itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
				OutputIndex: st.outputIndex,
				Item:        finalMsg,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
			s.closedItems = append(s.closedItems, finalMsg)

		case v1.ItemTypeFunctionCall:
			// R-3: patch callID and name from the completed item payload if available,
			// falling back to per-stream state populated from item.started.
			callID := st.callID
			name := st.name
			var fcProbe struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}
			var evItemRaw struct {
				Item json.RawMessage `json:"item"`
			}
			if json.Unmarshal(data, &evItemRaw) == nil && len(evItemRaw.Item) > 0 {
				if json.Unmarshal(evItemRaw.Item, &fcProbe) == nil {
					if fcProbe.CallID != "" {
						callID = fcProbe.CallID
					}
					if fcProbe.Name != "" {
						name = fcProbe.Name
					}
					if fcProbe.Arguments != "" {
						st.argsBuf = fcProbe.Arguments
					}
				}
			}
			argsDoneData, _ := json.Marshal(ResponsesFunctionCallArgumentsDoneEvent{
				ItemID:      itemID,
				OutputIndex: st.outputIndex,
				CallID:      callID,
				Arguments:   st.argsBuf,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventFunctionCallArgumentsDone, Data: argsDoneData})
			finalFC := &ResponsesFunctionCall{
				ID:        itemID,
				CallID:    callID,
				Name:      name,
				Arguments: st.argsBuf,
				Status:    ResponsesStatusCompleted,
			}
			itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
				OutputIndex: st.outputIndex,
				Item:        finalFC,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
			s.closedItems = append(s.closedItems, finalFC)

		case v1.ItemTypeReasoning:
			textDoneData, _ := json.Marshal(ResponsesReasoningTextDoneEvent{
				ItemID:       itemID,
				OutputIndex:  st.outputIndex,
				ContentIndex: 0,
				Text:         st.textBuf,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventReasoningTextDone, Data: textDoneData})
			finalR := &ResponsesReasoning{
				ID:      itemID,
				Status:  ResponsesStatusCompleted,
				Summary: []ResponsesSummaryText{{Text: st.textBuf}},
			}
			itemDoneData, _ := json.Marshal(ResponsesOutputItemDoneEvent{
				OutputIndex: st.outputIndex,
				Item:        finalR,
			})
			frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventOutputItemDone, Data: itemDoneData})
			s.closedItems = append(s.closedItems, finalR)
		}

	case v1.EventGenerationCompleted:
		var ev v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}

		finalResp := &ResponsesResponse{
			ID:           s.responseID,
			Object:       "response",
			CreatedAt:    s.created,
			Model:        s.model,
			Status:       ResponsesStatus(ev.Status),
			FinishReason: canonicalFinishReasonToResponses(ev.FinishReason),
			Output:       append([]ResponsesItem{}, s.closedItems...),
		}
		if ev.Usage != nil {
			finalResp.Usage = canonicalUsageToResponses(ev.Usage)
		}

		var finalEvent string
		if ev.Status == v1.StatusIncomplete {
			finalEvent = ResponsesEventIncomplete
		} else {
			finalEvent = ResponsesEventCompleted
		}
		completedData, _ := json.Marshal(ResponsesCompletedEvent{Response: finalResp})
		frames = append(frames, ResponsesSSEFrame{Event: finalEvent, Data: completedData})

	case v1.EventError:
		var ev v1.ErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, nil
		}
		errData, _ := json.Marshal(ResponsesErrorEvent{Code: ev.Code, Message: ev.Message})
		frames = append(frames, ResponsesSSEFrame{Event: ResponsesEventError, Data: errData})
	}

	return marshalResponsesFrames(frames), nil
}

// marshalResponsesFrames serializes a slice of ResponsesSSEFrame values to wire bytes.
func marshalResponsesFrames(frames []ResponsesSSEFrame) []byte {
	if len(frames) == 0 {
		return nil
	}
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
}

// composedStream chains a toCanonical function and a fromCanonical function:
// input chunk → toCanonical → canonical chunks → fromCanonical → output chunks.
// Both functions return []byte (concatenated SSE frames); we split them and
// feed each canonical frame into fromCanonical.
//
// Used to compose CCTranslator.NewToCanonicalStream with
// ResponsesTranslator.NewFromCanonicalStream.
type ComposedStream struct {
	toCanonical   func([]byte) ([]byte, error)
	fromCanonical func([]byte) ([]byte, error)
}

// NewComposedStream creates a ComposedStream from two translator stream functions.
func NewComposedStream(toCanonical, fromCanonical func([]byte) ([]byte, error)) *ComposedStream {
	return &ComposedStream{toCanonical: toCanonical, fromCanonical: fromCanonical}
}

// Translate processes one upstream chunk through the canonical chain and returns
// the translated output frames.
func (c *ComposedStream) Translate(chunk []byte) ([]ResponsesSSEFrame, error) {
	canonBytes, err := c.toCanonical(chunk)
	if err != nil {
		return nil, err
	}
	if len(canonBytes) == 0 {
		return nil, nil
	}

	// Split canonical bytes into individual SSE frames and feed each to fromCanonical.
	var out []ResponsesSSEFrame
	frames := splitSSEFrames(canonBytes)
	for _, frame := range frames {
		outBytes, err := c.fromCanonical(frame)
		if err != nil {
			return nil, err
		}
		// Parse the output bytes back into ResponsesSSEFrames.
		responseFrames := splitResponsesSSEFrames(outBytes)
		out = append(out, responseFrames...)
	}
	return out, nil
}

// splitSSEFrames splits concatenated SSE wire bytes into individual \n\n-delimited frames.
func splitSSEFrames(b []byte) [][]byte {
	var frames [][]byte
	for len(b) > 0 {
		idx := bytes.Index(b, []byte("\n\n"))
		if idx < 0 {
			if len(bytes.TrimSpace(b)) > 0 {
				frames = append(frames, append(b, '\n', '\n'))
			}
			break
		}
		frame := b[:idx+2]
		if len(bytes.TrimSpace(b[:idx])) > 0 {
			frames = append(frames, frame)
		}
		b = b[idx+2:]
	}
	return frames
}

// splitResponsesSSEFrames parses concatenated ResponsesSSEFrame wire bytes back into structs.
func splitResponsesSSEFrames(b []byte) []ResponsesSSEFrame {
	var frames []ResponsesSSEFrame
	for _, raw := range splitSSEFrames(b) {
		event, data, ok := ParseResponsesSSEChunk(raw)
		if !ok {
			continue
		}
		frames = append(frames, ResponsesSSEFrame{Event: event, Data: data})
	}
	return frames
}
