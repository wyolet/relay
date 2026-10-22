package openai

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	"github.com/wyolet/relay/sdk/internal/sse"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// SummaryFromJSONEnds implements v1.JSONEndsSummarizer.
func (ResponsesTranslator) SummaryFromJSONEnds(body []byte) (v1.Summary, bool) {
	return responsesSummaryFromEnds(body)
}

// SummaryFromStreamEnds implements v1.StreamEndsSummarizer. A stream's Summary is its last terminal event's, built from that event's own response object alone, so the terminal events are found by name and the last one is read like a sync body.
func (ResponsesTranslator) SummaryFromStreamEnds(body []byte) (v1.Summary, bool) {
	frames, ok := sse.EventFrames(body, ResponsesEventCompleted, ResponsesEventIncomplete, ResponsesEventFailed)
	if !ok {
		return v1.Summary{}, false
	}
	if len(frames) == 0 {
		return v1.Summary{}, true
	}
	sc := sse.NewScanner(frames[len(frames)-1])
	if !sc.Next() {
		return v1.Summary{}, false
	}
	resp, ok := terminalEventResponse(sc.Data())
	if !ok {
		return v1.Summary{}, false
	}
	sum, ok := responsesSummaryFromEnds(resp)
	// An empty Summary leaves an earlier terminal event's in place, which only the full walk can find.
	if !ok || (len(sum.Tokens) == 0 && sum.FinishReason == "" && sum.ServiceTier == "") {
		return v1.Summary{}, false
	}
	return sum, true
}

// terminalEventResponse returns the response object a terminal event carries, the one parseStreamTerminalResponse decodes.
func terminalEventResponse(data []byte) ([]byte, bool) {
	var resp []byte
	m := jsonscan.Object(data)
	for m.Next() {
		if jsonscan.KeyIs(m.Key(), "response") {
			resp = m.Value()
		}
	}
	if m.Err() || resp == nil || jsonscan.IsNull(resp) {
		return nil, false
	}
	return resp, true
}

// responsesEnds collects the members of a Responses object that responsesResponseToCanonical builds the Summary from, each with whether it was seen.
type responsesEnds struct {
	status                          ResponsesStatus
	incomplete                      *ResponsesIncompleteDetails
	tier                            string
	usage                           *ResponsesUsage
	statusSeen, incompleteSeen      bool
	tierSeen, usageSeen, outputSeen bool
	calls, refused                  bool
	// unscanned holds the output items no walk reached.
	unscanned []byte
}

// responsesSummaryFromEnds reads a Responses object's Summary. usage and service_tier follow output, so they are read walking back from the end together with the output items in reach; status and incomplete_details precede output and are read from the head when that walk stops short of them. What neither walk reaches, most of a long output, is only searched for the item and part types that change the finish reason and for the members that were not found.
func responsesSummaryFromEnds(body []byte) (v1.Summary, bool) {
	var r responsesEnds
	tail := jsonscan.ObjectFromEnd(body, len(body)-jsonscan.EndsWindow)
	for tail.Next() {
		output, ok := r.take(tail.Key(), tail.Value(), true)
		if !ok || (output && !r.scanOutputFromEnd(&tail)) {
			return v1.Summary{}, false
		}
	}
	if tail.Err() && !r.readHead(body, len(tail.Unread())) {
		return v1.Summary{}, false
	}
	finish, ok := r.finishReason()
	if !ok {
		return v1.Summary{}, false
	}
	return v1.Summary{Tokens: responsesUsageToCanonical(r.usage), FinishReason: finish, ServiceTier: r.tier}, true
}

// take decodes the member k: v into r when the Summary reads it, reporting whether it is a non-null output, which the caller scans, and false when it does not decode (the full decode fails on it too). keep leaves a member already taken as it is: walking from the end meets the last of duplicate members first.
func (r *responsesEnds) take(k, v []byte, keep bool) (output, ok bool) {
	var dst any
	var seen *bool
	switch {
	case jsonscan.KeyIs(k, "status"):
		dst, seen = &r.status, &r.statusSeen
	case jsonscan.KeyIs(k, "incomplete_details"):
		dst, seen = &r.incomplete, &r.incompleteSeen
	case jsonscan.KeyIs(k, "service_tier"):
		dst, seen = &r.tier, &r.tierSeen
	case jsonscan.KeyIs(k, "usage"):
		dst, seen = &r.usage, &r.usageSeen
	case jsonscan.KeyIs(k, "output"):
		if keep && r.outputSeen {
			return false, true
		}
		r.outputSeen, r.calls, r.refused, r.unscanned = true, false, false, nil
		return !jsonscan.IsNull(v), true
	default:
		return false, true
	}
	if keep && *seen {
		return false, true
	}
	*seen = true
	return false, json.Unmarshal(v, dst) == nil
}

// scanOutputFromEnd reads the output items of the member m is on from the last back, until a tool call settles the finish reason or the window ends.
func (r *responsesEnds) scanOutputFromEnd(m *jsonscan.MembersFromEnd) bool {
	items := m.ValueArray()
	for items.Next() {
		call, refusal, ok := responsesItemFinish(items.Value())
		if !ok {
			return false
		}
		if call {
			r.calls = true
			return true
		}
		r.refused = r.refused || refusal
	}
	if items.Err() {
		r.unscanned = items.Unread()
	}
	return true
}

// readHead reads, from the members at the head of body, those the walk from the end did not reach before it stopped at tailStart. Members neither walk reached lie between the two; a member still missing must be shown absent there.
func (r *responsesEnds) readHead(body []byte, tailStart int) bool {
	var h responsesEnds
	limit := min(len(body), jsonscan.EndsWindow)
	head := jsonscan.Object(body[:limit])
	for head.Next() {
		output, ok := h.take(head.Key(), head.Value(), false)
		if !ok || (output && !h.scanOutput(head.Value())) {
			return false
		}
	}
	headEnd := min(limit-len(head.Rest()), tailStart)
	gap := body[headEnd:tailStart]
	if !r.statusSeen {
		r.status, r.statusSeen = h.status, h.statusSeen
	}
	if !r.incompleteSeen {
		r.incomplete, r.incompleteSeen = h.incomplete, h.incompleteSeen
	}
	if !r.tierSeen {
		r.tier, r.tierSeen = h.tier, h.tierSeen
	}
	if !r.usageSeen {
		r.usage, r.usageSeen = h.usage, h.usageSeen
	}
	if h.outputSeen {
		// The head holds output whole; it covers whatever items the walk from the end left unread.
		r.outputSeen, r.calls, r.refused = true, r.calls || h.calls, r.refused || h.refused
	}
	if r.unscanned != nil {
		r.unscanned = r.unscanned[min(headEnd, len(r.unscanned)):]
	}
	var absent []string
	for name, seen := range map[string]bool{"status": r.statusSeen, "incomplete_details": r.incompleteSeen, "service_tier": r.tierSeen, "usage": r.usageSeen} {
		if !seen {
			absent = append(absent, name)
		}
	}
	if !r.outputSeen && r.status == ResponsesStatusCompleted {
		// Output, if any, lies in the gap, and no call item has been seen.
		absent = append(absent, finishItemTypes...)
	}
	return len(absent) == 0 || !(jsonscan.HasLetterEscape(gap) || jsonscan.MayHoldString(gap, absent...))
}

// scanOutput reads every output item of a complete output array.
func (r *responsesEnds) scanOutput(output []byte) bool {
	items := jsonscan.Array(output)
	for items.Next() {
		call, refusal, ok := responsesItemFinish(items.Value())
		if !ok {
			return false
		}
		r.calls = r.calls || call
		r.refused = r.refused || refusal
	}
	return !items.Err()
}

// finishReason is responsesCanonicalFinishReason over what was read; false when output items left unscanned could change it.
func (r *responsesEnds) finishReason() (v1.FinishReason, bool) {
	switch r.status {
	case ResponsesStatusCompleted:
		if !r.calls && len(r.unscanned) > 0 && (jsonscan.HasLetterEscape(r.unscanned) || jsonscan.MayHoldString(r.unscanned, finishItemTypes...)) {
			return "", false
		}
		switch {
		case r.calls:
			return v1.FinishReasonToolCalls, true
		case r.refused:
			return v1.FinishReasonRefusal, true
		}
		return v1.FinishReasonStop, true
	case ResponsesStatusIncomplete:
		var reason string
		if r.incomplete != nil {
			reason = r.incomplete.Reason
		}
		switch reason {
		case "content_filter":
			return v1.FinishReasonContentFilter, true
		case "max_output_tokens":
			return v1.FinishReasonLength, true
		}
		return v1.FinishReasonOther, true
	}
	return "", true
}

// finishItemTypes are the item and part types that change a completed response's finish reason; output not walked is searched for them, together with escapes that could spell them.
var finishItemTypes = []string{string(ResponsesItemTypeFunctionCall), string(ResponsesItemTypeCustomToolCall), string(ResponsesPartTypeRefusal)}

// responsesItemFinish reads what one output item contributes to the finish reason, the way responsesUnmarshalItem types it: a function or custom tool call, or a message holding a refusal part. false means the item does not decode as an output item.
func responsesItemFinish(item []byte) (call, refusal, ok bool) {
	if jsonscan.IsNull(item) {
		return false, false, true
	}
	var (
		typ     ResponsesItemType
		content []byte
	)
	m := jsonscan.Object(item)
	for m.Next() {
		switch k, v := m.Key(), m.Value(); {
		case jsonscan.KeyIs(k, "type"):
			if json.Unmarshal(v, &typ) != nil {
				return false, false, false
			}
		case jsonscan.KeyIs(k, "content"):
			content = v
		}
	}
	if m.Err() {
		return false, false, false
	}
	switch typ {
	case ResponsesItemTypeFunctionCall, ResponsesItemTypeCustomToolCall:
		return true, false, true
	case ResponsesItemTypeMessage:
		refusal, ok = responsesContentRefuses(content)
		return false, refusal, ok
	}
	return false, false, true
}

// responsesContentRefuses reports whether message content holds a refusal part, read the way responsesUnmarshalContent types the parts.
func responsesContentRefuses(content []byte) (refusal, ok bool) {
	if len(content) == 0 || content[0] == '"' || jsonscan.IsNull(content) {
		return false, true
	}
	parts := jsonscan.Array(content)
	for parts.Next() {
		var typ ResponsesPartType
		p := jsonscan.Object(parts.Value())
		for p.Next() {
			if jsonscan.KeyIs(p.Key(), "type") && json.Unmarshal(p.Value(), &typ) != nil {
				return false, false
			}
		}
		if p.Err() {
			return false, false
		}
		refusal = refusal || typ == ResponsesPartTypeRefusal
	}
	return refusal, !parts.Err()
}
