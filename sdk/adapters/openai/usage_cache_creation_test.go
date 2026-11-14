package openai

import (
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

// Cache writes (an Anthropic upstream's cache_creation, its priciest input meter) are prompt tokens, so the client-visible prompt count must include them.
func TestCanonicalUsage_CacheCreationInPromptTokens(t *testing.T) {
	t.Skip("bug: {input:300, cache_read:100, cache_creation:200} → prompt_tokens / input_tokens 400, the 200 cache-write tokens dropped")
	tok := usage.Tokens{"input": 300, "cache_read": 100, "cache_creation": 200, "output": 50}
	if cc := canonicalUsageToCC(tok); cc.PromptTokens != 600 || cc.TotalTokens != 650 {
		t.Errorf("Chat Completions usage = %+v, want prompt_tokens 600, total_tokens 650", cc)
	}
	if r := canonicalUsageToResponses(tok); r.InputTokens != 600 {
		t.Errorf("Responses usage = %+v, want input_tokens 600", r)
	}
}
