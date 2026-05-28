package usage

// Tokens is the universal token-count shape across providers.
// Keys are convention-driven, not enforced. Common values:
//
//	input             prompt tokens (all providers)
//	output            completion tokens (all providers)
//	cache_creation    Anthropic prompt-cache write
//	cache_read        Anthropic prompt-cache hit
//	reasoning         OpenAI o-series + Anthropic extended thinking
//	server_tool_use   Anthropic server-side tool calls
//
// Per-shape parsers fill whatever keys their provider returned. Sum over
// the map gives a backward-compatible "total tokens" for legacy consumers.
type Tokens map[string]int64

// Add adds other into t in place. Useful for streaming chunks where each
// chunk emits a partial usage block.
func (t Tokens) Add(other Tokens) {
	for k, v := range other {
		t[k] += v
	}
}

// Sum returns the total of all values. Used by legacy single-meter
// rate-limit callers that haven't migrated to typed meters.
func (t Tokens) Sum() int64 {
	var s int64
	for _, v := range t {
		s += v
	}
	return s
}
