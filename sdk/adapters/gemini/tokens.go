package gemini

import (
	"bytes"
	"encoding/json"

	"github.com/wyolet/relay/sdk/usage"
)

// ExtractTokens reads Gemini usage from a generateContent response body. It
// accepts either a single non-streaming JSON object or a complete streaming SSE
// body. For streaming, we walk every `data:` frame and keep the last one that
// carries usageMetadata.
// Maps:
//
//	promptTokenCount - cachedContentTokenCount -> input
//	candidatesTokenCount                       -> output
//	cachedContentTokenCount                    -> cache_read
//	thoughtsTokenCount                         -> reasoning
//
// Dimensions are orthogonal (input excludes cache_read). Returns nil when
// usageMetadata is absent or all counts are zero.
func ExtractTokens(body []byte) usage.Tokens {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '{' {
		return extractTokensObject(trimmed)
	}
	var last usage.Tokens
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || payload[0] != '{' {
			continue
		}
		if frame := extractTokensObject(payload); frame != nil {
			last = frame
		}
	}
	return last
}

func extractTokensObject(body []byte) usage.Tokens {
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
	cached := u.CachedContentTokenCount
	if v := u.PromptTokenCount - cached; v > 0 {
		t["input"] = int64(v)
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
