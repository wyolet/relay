package openai

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

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
	}
	if rresp.CreatedAt == 0 {
		rresp.CreatedAt = time.Now().Unix()
	}

	// Responses has no finish_reason field: derive status + incomplete_details.
	rresp.Status, rresp.IncompleteDetails = canonicalResponsesStatus(resp.Status, resp.FinishReason, resp.IncompleteDetails)

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

	// incomplete_details is set above via canonicalResponsesStatus.
	if resp.Error != nil {
		rresp.Error = &ResponsesError{
			Code:    resp.Error.Code,
			Message: resp.Error.Message,
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

// parseStreamTerminalResponse extracts the `response` object from a terminal
// streaming event (response.completed / .incomplete / .failed) using the
// polymorphic-aware UnmarshalResponsesResponse. The event's response.output is
// a []ResponsesItem interface slice that plain json.Unmarshal cannot build, so
// the terminal event MUST go through the custom unmarshaler — otherwise the
// error is swallowed and usage/finish_reason never reach the caller.
func parseStreamTerminalResponse(data []byte) *ResponsesResponse {
	var ev struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil
	}
	if len(ev.Response) == 0 || string(ev.Response) == "null" {
		return nil
	}
	resp, err := UnmarshalResponsesResponse(ev.Response)
	if err != nil {
		return nil
	}
	return resp
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
	cr.FinishReason = responsesCanonicalFinishReason(resp)

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

// responsesCanonicalFinishReason derives a canonical finish_reason from a
// Responses response. The Responses API has NO finish_reason field — terminal
// state is carried by status + incomplete_details.reason. Reading a (nonexistent)
// wire finish_reason always yielded "" → defaulted to stop, so truncation and
// content-filter cutoffs masqueraded as clean stops (canonical rule 11).
func responsesCanonicalFinishReason(resp *ResponsesResponse) v1.FinishReason {
	switch resp.Status {
	case ResponsesStatusCompleted:
		for _, it := range resp.Output {
			if it != nil && it.ResponsesItemType() == ResponsesItemTypeFunctionCall {
				return v1.FinishReasonToolCalls
			}
		}
		return v1.FinishReasonStop
	case ResponsesStatusIncomplete:
		if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason == "content_filter" {
			return v1.FinishReasonContentFilter
		}
		// max_output_tokens or any other incomplete reason → truncated.
		return v1.FinishReasonLength
	default:
		// failed / cancelled / queued / in_progress: not a clean stop. The
		// signal rides Status + Error; do not fabricate a finish_reason.
		return ""
	}
}

// canonicalResponsesStatus derives the Responses wire status + incomplete_details
// from a canonical finish_reason/status. The inverse of
// responsesCanonicalFinishReason: length/content_filter become status=incomplete
// with the matching incomplete_details.reason (the only way Responses expresses
// them — it has no finish_reason field).
func canonicalResponsesStatus(status v1.Status, fr v1.FinishReason, inc *v1.IncompleteDetails) (ResponsesStatus, *ResponsesIncompleteDetails) {
	switch fr {
	case v1.FinishReasonLength:
		reason := "max_output_tokens"
		if inc != nil && inc.Reason != "" {
			reason = inc.Reason
		}
		return ResponsesStatusIncomplete, &ResponsesIncompleteDetails{Reason: reason}
	case v1.FinishReasonContentFilter:
		reason := "content_filter"
		if inc != nil && inc.Reason != "" {
			reason = inc.Reason
		}
		return ResponsesStatusIncomplete, &ResponsesIncompleteDetails{Reason: reason}
	}
	// stop / tool_calls / refusal / empty: honor an explicit non-completed
	// canonical status (failed/cancelled/queued from a Responses-native
	// upstream) verbatim; otherwise the response completed.
	if status != "" && status != v1.StatusCompleted {
		var id *ResponsesIncompleteDetails
		if inc != nil {
			id = &ResponsesIncompleteDetails{Reason: inc.Reason}
		}
		return ResponsesStatus(status), id
	}
	return ResponsesStatusCompleted, nil
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
		InputTokens:  input,
		OutputTokens: output,
		// input + output, NOT Tokens.Sum(): reasoning is a sub-breakdown
		// already inside output_tokens, so summing the map double-counts it.
		TotalTokens:        input + output,
		InputTokensDetails: ResponsesInputDeets{CachedTokens: cached},
	}
	if reasoning := int(t["reasoning"]); reasoning > 0 {
		r.OutputTokensDetails = ResponsesOutputDeets{ReasoningTokens: reasoning}
	}
	return r
}

// --- Responses → canonical stream ---
