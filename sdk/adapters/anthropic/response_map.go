package anthropic

import (
	"encoding/json"
	"strings"

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
	return anthropicTokens(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, u.CacheCreationByTTL.Ephemeral1hInputTokens)
}

// anthropicTokens builds the canonical Tokens map from the counters
// Anthropic reports, skipping zero meters and returning nil when none are set.
// cacheCreation1h is the 1-hour share of cacheCreation, billed at its own rate.
func anthropicTokens(input, output, cacheRead, cacheCreation, cacheCreation1h int) usage.Tokens {
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
	if cacheCreation1h > 0 {
		t["cache_creation_1h"] = int64(cacheCreation1h)
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
		return v1.StatusIncomplete, v1.FinishReasonOther, &v1.IncompleteDetails{Reason: unknownStopReasonPrefix + reason}
	}
}

// unknownStopReasonPrefix marks incomplete_details.reason for a stop_reason
// this adapter does not recognise.
const unknownStopReasonPrefix = "anthropic:"

// extStopReason carries the raw stop_reason when canonical has no value for it.
const extStopReason = "anthropic.stop_reason"

// canonicalFinishReasonToAnthropic maps canonical status + finish_reason + incomplete_details to Anthropic stop_reason string.
func canonicalFinishReasonToAnthropic(status v1.Status, reason v1.FinishReason, incomplete *v1.IncompleteDetails) string {
	if status == v1.StatusFailed && reason == "" {
		// canonical: status=failed with no finish_reason must not surface as end_turn
		// (rule 11); refusal is the nearest non-success Anthropic stop_reason.
		return "refusal"
	}
	if incomplete != nil {
		switch incomplete.Reason {
		case "max_output_tokens":
			return "max_tokens"
		case "pause_turn":
			return "pause_turn"
		}
		// An unrecognised upstream stop_reason goes back out verbatim rather
		// than defaulting to end_turn.
		if raw, ok := strings.CutPrefix(incomplete.Reason, unknownStopReasonPrefix); ok && raw != "" {
			return raw
		}
	}
	if status == v1.StatusIncomplete && reason == "" {
		// Unfinished with no stated cause: pause_turn is the stop_reason that parses back to this shape.
		return "pause_turn"
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
	case v1.FinishReasonOther:
		// Another wire's unknown reason: refusal is the one terminal non-success
		// stop_reason; max_tokens and pause_turn would invite a continuation.
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicCitationsToCanonical maps text-block citations to canonical Annotations.
// Web search results become URL citations; document and search-result locations
// (char, page, content block) have no canonical type and ride verbatim as RawAnnotation.
func anthropicCitationsToCanonical(cits []anthropicCitation) []v1.Annotation {
	if len(cits) == 0 {
		return nil
	}
	out := make([]v1.Annotation, 0, len(cits))
	for _, c := range cits {
		switch c.Type {
		case "url_citation":
			out = append(out, &v1.URLCitationAnnotation{
				URL:        c.URL,
				Title:      c.Title,
				StartIndex: c.StartIndex,
				EndIndex:   c.EndIndex,
			})
		case "web_search_result_location":
			// The whole citation rides in ProviderData: cited_text and encrypted_index have no canonical field, and replay needs both.
			out = append(out, &v1.URLCitationAnnotation{URL: c.URL, Title: c.Title, ProviderData: c.raw})
		default:
			out = append(out, &v1.RawAnnotation{Type: c.Type, JSON: c.raw})
		}
	}
	return out
}

// canonicalAnnotationsToAnthropic writes back the Anthropic citations canonical carries verbatim; nil when none are left.
func canonicalAnnotationsToAnthropic(anns []v1.Annotation) []json.RawMessage {
	var out []json.RawMessage
	for _, a := range anns {
		switch v := a.(type) {
		case *v1.URLCitationAnnotation:
			if isAnthropicCitation(v.ProviderData) {
				out = append(out, v.ProviderData)
			}
			// canonical: URL citation without Anthropic provider_data dropped — web_search_result_location needs the encrypted_index only Anthropic issues, and Anthropic has no plain URL citation.
		case *v1.RawAnnotation:
			if isAnthropicLocation(v.Type) && len(v.JSON) > 0 {
				out = append(out, v.JSON)
			}
			// canonical: raw annotation of another vendor dropped — Anthropic accepts only its own citation types.
		case *v1.TextCitationAnnotation:
			// canonical: text_citation dropped — Anthropic locations name a document and its cited text, which output-text offsets cannot rebuild.
		}
	}
	return out
}

func isAnthropicLocation(typ string) bool {
	switch typ {
	case "char_location", "page_location", "content_block_location", "search_result_location", "web_search_result_location":
		return true
	}
	return false
}

// isAnthropicCitation reports whether provider_data holds a citation an Anthropic parse left there, not another vendor's blob.
func isAnthropicCitation(providerData json.RawMessage) bool {
	var pd struct {
		Type string `json:"type"`
	}
	return len(providerData) > 0 && json.Unmarshal(providerData, &pd) == nil && isAnthropicLocation(pd.Type)
}
