package gemini

import (
	"bytes"
	"encoding/json"

	"github.com/wyolet/relay/sdk/internal/sse"
	"github.com/wyolet/relay/sdk/usage"
)

// ExtractTokens reads Gemini usage from a generateContent response body. It
// accepts either a single non-streaming JSON object or a complete streaming SSE
// body. For streaming, we walk every `data:` frame and keep the last one that
// carries usageMetadata.
// Maps:
//
//	promptTokenCount - cachedContentTokenCount -> input
//	candidatesTokenCount + thoughtsTokenCount  -> output
//	cachedContentTokenCount                    -> cache_read
//	thoughtsTokenCount                         -> reasoning
//
// Input excludes cache_read. Gemini counts thoughts apart from candidates, while canonical output includes reasoning (usage.Tokens), so output is their sum. Returns nil when
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
	for sc := sse.NewScanner(body); sc.Next(); {
		payload := bytes.TrimSpace(sc.Data())
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
	if v := u.CandidatesTokenCount + u.ThoughtsTokenCount; v > 0 {
		t["output"] = int64(v)
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

// canonicalUsageToGemini is the inverse of geminiUsageToTokens: the prompt count includes the cached tokens, and candidates are the output tokens that are not thoughts.
func canonicalUsageToGemini(t usage.Tokens) map[string]int64 {
	um := map[string]int64{}
	reasoning := t["reasoning"]
	if v := t["input"] + t["cache_read"]; v > 0 {
		um["promptTokenCount"] = v
	}
	if v := t["output"] - reasoning; v > 0 {
		um["candidatesTokenCount"] = v
	}
	if v := t["cache_read"]; v > 0 {
		um["cachedContentTokenCount"] = v
	}
	if reasoning > 0 {
		um["thoughtsTokenCount"] = reasoning
	}
	return um
}
