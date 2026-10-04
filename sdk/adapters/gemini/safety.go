package gemini

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Response.Extensions keys for Gemini safety output. Ratings have no canonical
// field, so they ride the envelope (rule 7) and SerializeResponse re-emits them.
const (
	extSafetyRatings  = "gemini.safety_ratings"
	extPromptFeedback = "gemini.prompt_feedback"
)

// promptBlockReason returns promptFeedback.blockReason, or "" when the prompt
// was not blocked.
func promptBlockReason(feedback json.RawMessage) string {
	if len(feedback) == 0 {
		return ""
	}
	var pf struct {
		BlockReason string `json:"blockReason"`
	}
	if err := json.Unmarshal(feedback, &pf); err != nil {
		return ""
	}
	return pf.BlockReason
}

// promptBlocked is the canonical terminal state of a blocked prompt: no
// candidate exists, so it must surface as a content filter, not an empty
// success (rule 11).
func promptBlocked() (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	return v1.StatusIncomplete, v1.FinishReasonContentFilter, &v1.IncompleteDetails{Reason: "content_filter"}
}

// safetyExtensions carries the prompt feedback and the first candidate's
// safety ratings, or nil when the upstream sent neither.
func safetyExtensions(gr *geminiResponse) map[string]json.RawMessage {
	ext := map[string]json.RawMessage{}
	if len(gr.PromptFeedback) > 0 && string(gr.PromptFeedback) != "null" {
		ext[extPromptFeedback] = gr.PromptFeedback
	}
	if len(gr.Candidates) > 0 {
		if r := gr.Candidates[0].SafetyRatings; len(r) > 0 && string(r) != "null" {
			ext[extSafetyRatings] = r
		}
	}
	if len(ext) == 0 {
		return nil
	}
	return ext
}
