package openai

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/sdk/usage"
)

// Cache writes (an Anthropic upstream's cache_creation, its priciest input meter) are prompt tokens, so the client-visible prompt count must include them.
func TestCanonicalUsage_CacheCreationInPromptTokens(t *testing.T) {
	tok := usage.Tokens{"input": 300, "cache_read": 100, "cache_creation": 200, "output": 50}
	wantCC := &Usage{PromptTokens: 600, CompletionTokens: 50, TotalTokens: 650, PromptDetails: &PromptTokenDetails{CachedTokens: 100}}
	if d := cmp.Diff(wantCC, canonicalUsageToCC(tok)); d != "" {
		t.Errorf("Chat Completions usage (-want +got):\n%s", d)
	}
	wantResponses := &ResponsesUsage{InputTokens: 600, OutputTokens: 50, TotalTokens: 650, InputTokensDetails: ResponsesInputDeets{CachedTokens: 100}}
	if d := cmp.Diff(wantResponses, canonicalUsageToResponses(tok)); d != "" {
		t.Errorf("Responses usage (-want +got):\n%s", d)
	}
}
