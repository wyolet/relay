package gemini

import (
	"encoding/json"

	"github.com/wyolet/relay/pkg/usage"
)

// ExtractTokens reads Gemini usage from a generateContent response body.
// Maps:
//
//	promptTokenCount         -> input
//	candidatesTokenCount     -> output
//	cachedContentTokenCount  -> cache_read
//	thoughtsTokenCount       -> reasoning
//
// Dimensions are orthogonal (non-overlapping). Returns nil when usageMetadata
// is absent or all counts are zero.
func ExtractTokens(body []byte) usage.Tokens {
	var resp struct {
		UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.UsageMetadata == nil {
		return nil
	}
	return geminiUsageToTokens(resp.UsageMetadata)
}

func geminiUsageToTokens(u *usageMetadata) usage.Tokens {
	if u == nil {
		return nil
	}
	t := usage.Tokens{}
	if u.PromptTokenCount > 0 {
		t["input"] = int64(u.PromptTokenCount)
	}
	if u.CandidatesTokenCount > 0 {
		t["output"] = int64(u.CandidatesTokenCount)
	}
	if u.CachedContentTokenCount > 0 {
		t["cache_read"] = int64(u.CachedContentTokenCount)
	}
	if u.ThoughtsTokenCount > 0 {
		t["reasoning"] = int64(u.ThoughtsTokenCount)
	}
	if len(t) == 0 {
		return nil
	}
	return t
}
