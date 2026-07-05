// Bug-reproduction tests for the 2026-07-04 audit (audit-sdk-adapters.md).
// Each test asserts the CORRECT wire behavior and is expected to FAIL until
// the corresponding finding is fixed; once red is confirmed they are t.Skip'd
// with an audit marker so the suite stays green.
package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

// Audit P3 (DECISION #4): cache_creation tokens must remain visible in the
// client-facing usage payload. An Anthropic upstream reports cache-writes as
// a separate meter (canonical "cache_creation", billed at 1.25x-2x the input
// rate); when that response is egressed through the CC or Responses shape,
// canonicalUsageToCC/canonicalUsageToResponses reconstruct the prompt side
// from input + cache_read only, so cache-creation tokens vanish from the
// client-visible usage block entirely — client-side cost accounting
// undercounts exactly the most expensive meter, with no greppable drop
// annotation (rule 11).
//
// Token conservation: the client-visible input side must account for all
// prompt tokens the provider processed (input + cache_read + cache_creation),
// either folded into prompt_tokens/input_tokens or exposed as an explicit
// field carrying "cache_creation".
func TestCanonicalUsage_CacheCreationVisibleToClient(t *testing.T) {
	t.Skip("audit 2026-07-04: cache_creation tokens vanish from client-visible CC/Responses usage — known-broken, unskip with the fix")
	// As produced by anthropicUsageToCanonical for a cache-write turn:
	// 300 uncached input + 100 cache read + 200 cache creation + 50 output.
	tok := usage.Tokens{"input": 300, "cache_read": 100, "cache_creation": 200, "output": 50}
	const wantPromptSide = 300 + 100 + 200

	t.Run("cc", func(t *testing.T) {
		cc := canonicalUsageToCC(tok)
		if cc == nil {
			t.Fatal("nil usage")
		}
		wire, _ := json.Marshal(cc)
		if cc.PromptTokens != wantPromptSide && !strings.Contains(string(wire), "cache_creation") {
			t.Errorf("cache_creation (200 tokens) dropped from client-visible CC usage: prompt_tokens = %d, want %d (input+cache_read+cache_creation) or an explicit cache_creation field\nwire usage: %s",
				cc.PromptTokens, wantPromptSide, wire)
		}
	})

	t.Run("responses", func(t *testing.T) {
		rr := canonicalUsageToResponses(tok)
		if rr == nil {
			t.Fatal("nil usage")
		}
		wire, _ := json.Marshal(rr)
		if rr.InputTokens != wantPromptSide && !strings.Contains(string(wire), "cache_creation") {
			t.Errorf("cache_creation (200 tokens) dropped from client-visible Responses usage: input_tokens = %d, want %d (input+cache_read+cache_creation) or an explicit cache_creation field\nwire usage: %s",
				rr.InputTokens, wantPromptSide, wire)
		}
	})
}
