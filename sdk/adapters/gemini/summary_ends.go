package gemini

import (
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// SummaryFromJSONEnds implements v1.JSONEndsSummarizer. usageMetadata trails the body, but the finish reason is candidates[0]'s, set ahead of usage and possibly promoted by any functionCall part in its content, so a body is read only when the window covers it whole; even then only the members the Summary needs are decoded.
func (GeminiTranslator) SummaryFromJSONEnds(body []byte) (v1.Summary, bool) {
	if len(body) > jsonscan.EndsWindow {
		return v1.Summary{}, false
	}
	var (
		usage      *usageMetadata
		candidates []byte
		feedback   json.RawMessage
	)
	m := jsonscan.Object(body)
	for m.Next() {
		var err error
		switch k, v := m.Key(), m.Value(); {
		case jsonscan.KeyIs(k, "usageMetadata"):
			err = json.Unmarshal(v, &usage)
		case jsonscan.KeyIs(k, "candidates"):
			candidates = v
		case jsonscan.KeyIs(k, "promptFeedback"):
			feedback = json.RawMessage(v)
		}
		if err != nil {
			return v1.Summary{}, false
		}
	}
	if m.Err() {
		return v1.Summary{}, false
	}
	sum := v1.Summary{Tokens: geminiUsageToTokens(usage)}
	first, found, ok := firstCandidate(candidates)
	switch {
	case !ok:
		return v1.Summary{}, false
	case found:
		reason, hasFunctionCall, ok := candidateFinish(first)
		if !ok {
			return v1.Summary{}, false
		}
		_, sum.FinishReason, _ = geminiFinishReasonToCanonical(reason, hasFunctionCall)
	case promptBlockReason(feedback) != "":
		_, sum.FinishReason, _ = promptBlocked()
	}
	return sum, true
}

// firstCandidate returns the raw candidates[0] and whether the array has any element.
func firstCandidate(candidates []byte) (first []byte, found, ok bool) {
	if candidates == nil || jsonscan.IsNull(candidates) {
		return nil, false, true
	}
	e := jsonscan.Array(candidates)
	if !e.Next() {
		return nil, false, !e.Err()
	}
	return e.Value(), true, true
}

// candidateFinish reads a candidate's finishReason and whether its content holds a functionCall part, as ParseResponse decodes them.
func candidateFinish(cand []byte) (reason string, hasFunctionCall, ok bool) {
	if jsonscan.IsNull(cand) {
		return "", false, true
	}
	var content []byte
	m := jsonscan.Object(cand)
	for m.Next() {
		switch k, v := m.Key(), m.Value(); {
		case jsonscan.KeyIs(k, "finishReason"):
			if json.Unmarshal(v, &reason) != nil {
				return "", false, false
			}
		case jsonscan.KeyIs(k, "content"):
			content = v
		}
	}
	if m.Err() {
		return "", false, false
	}
	hasFunctionCall, ok = contentCallsFunction(content)
	return reason, hasFunctionCall, ok
}

// contentCallsFunction reports whether a candidate's content has a part whose functionCall decodes non-null.
func contentCallsFunction(content []byte) (calls, ok bool) {
	if content == nil || jsonscan.IsNull(content) {
		return false, true
	}
	var parts []byte
	m := jsonscan.Object(content)
	for m.Next() {
		if jsonscan.KeyIs(m.Key(), "parts") {
			parts = m.Value()
		}
	}
	if m.Err() {
		return false, false
	}
	if parts == nil || jsonscan.IsNull(parts) {
		return false, true
	}
	e := jsonscan.Array(parts)
	for e.Next() {
		if jsonscan.IsNull(e.Value()) {
			continue
		}
		call := false
		p := jsonscan.Object(e.Value())
		for p.Next() {
			if jsonscan.KeyIs(p.Key(), "functionCall") {
				call = !jsonscan.IsNull(p.Value())
			}
		}
		if p.Err() {
			return false, false
		}
		calls = calls || call
	}
	return calls, !e.Err()
}
