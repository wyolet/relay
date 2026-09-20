package anthropic

import (
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// anthropicUsageToCanonical maps Anthropic's response usage block to
// the canonical orthogonal-meter Tokens map. Keys match
// pricing.MeterForUsageKey so the same vocabulary flows from this
// adapter through every downstream observer + pricing computation.
func anthropicUsageToCanonical(u *anthropicFullUsage) usage.Tokens {
	if u == nil {
		return nil
	}
	return anthropicTokens(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens)
}

// anthropicTokens builds the canonical Tokens map from the four counters
// Anthropic reports, skipping zero meters and returning nil when none are set.
func anthropicTokens(input, output, cacheRead, cacheCreation int) usage.Tokens {
	t := usage.Tokens{}
	if input > 0 {
		t["input"] = int64(input)
	}
	if output > 0 {
		t["output"] = int64(output)
	}
	if cacheRead > 0 {
		t["cache_read"] = int64(cacheRead)
	}
	if cacheCreation > 0 {
		t["cache_creation"] = int64(cacheCreation)
	}
	if len(t) == 0 {
		return nil
	}
	return t
}

// anthropicStopReasonToCanonical maps an Anthropic stop_reason to canonical status/finish/incomplete.
func anthropicStopReasonToCanonical(reason string) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "max_tokens":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_output_tokens"}
	case "tool_use":
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	case "refusal":
		return v1.StatusCompleted, v1.FinishReasonRefusal, nil
	case "pause_turn":
		return v1.StatusIncomplete, "", &v1.IncompleteDetails{Reason: "pause_turn"}
	default:
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	}
}

// canonicalFinishReasonToAnthropic maps canonical finish_reason + incomplete_details to Anthropic stop_reason string.
func canonicalFinishReasonToAnthropic(reason v1.FinishReason, incomplete *v1.IncompleteDetails) string {
	if incomplete != nil {
		switch incomplete.Reason {
		case "max_output_tokens":
			return "max_tokens"
		case "pause_turn":
			return "pause_turn"
		}
	}
	return canonicalFinishReasonToAnthropicStr(reason)
}

func canonicalFinishReasonToAnthropicStr(reason v1.FinishReason) string {
	switch reason {
	case v1.FinishReasonStop:
		return "end_turn"
	case v1.FinishReasonLength:
		return "max_tokens"
	case v1.FinishReasonToolCalls:
		return "tool_use"
	case v1.FinishReasonRefusal:
		return "refusal"
	case v1.FinishReasonContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicCitationsToCanonical maps Anthropic url_citation annotations to canonical Annotations.
func anthropicCitationsToCanonical(cits []anthropicCitation) []v1.Annotation {
	if len(cits) == 0 {
		return nil
	}
	var out []v1.Annotation
	for _, c := range cits {
		if c.Type == "url_citation" {
			out = append(out, &v1.URLCitationAnnotation{
				URL:        c.URL,
				Title:      c.Title,
				StartIndex: c.StartIndex,
				EndIndex:   c.EndIndex,
			})
		}
		// char_location and page_location dropped — no clean v1 equivalent.
	}
	return out
}

// canonicalAnnotationsToAnthropic maps canonical annotations to Anthropic citations.
func canonicalAnnotationsToAnthropic(anns []v1.Annotation) []map[string]any {
	var out []map[string]any
	for _, a := range anns {
		switch v := a.(type) {
		case *v1.URLCitationAnnotation:
			out = append(out, map[string]any{
				"type":        "url_citation",
				"url":         v.URL,
				"title":       v.Title,
				"start_index": v.StartIndex,
				"end_index":   v.EndIndex,
			})
		}
	}
	return out
}
