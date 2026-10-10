// Package usage holds the pure wire shapes for usage reporting, token counts and upstream timing, shared by the public client and the server's usage records.
package usage

// Tokens is the universal token-count shape across providers. Keys are convention-driven, not enforced; every adapter fills the keys its provider reported with the same meaning:
//
//	input                   prompt tokens neither read from nor written to a prompt cache
//	cache_read              prompt tokens read from a cache (not in input)
//	cache_creation          prompt tokens written to a cache (not in input)
//	cache_creation_1h       part of cache_creation: prompt tokens written to a 1-hour cache
//	output                  every generated token, reasoning included
//	reasoning               part of output: reasoning tokens, when the provider reports them apart
//	audio_output            part of output: audio tokens
//	accepted_prediction     part of output: predicted-output tokens that appeared in the completion
//	rejected_prediction     part of output: predicted-output tokens that did not
//	audio_input             part of input: audio tokens
//	server_tool_use_input   input tokens of server-side tool calls; not treated as part of another key
//	server_tool_use_output  output tokens of server-side tool calls; not treated as part of another key
//
// A request's total is input + cache_read + cache_creation + output. A part is never added to its whole; Billable gives the count to charge once a part has a rate of its own.
type Tokens map[string]int64

// parts lists each key that is counted inside another, with the key that contains it.
var parts = [...]struct{ part, whole string }{
	{"reasoning", "output"},
	{"audio_output", "output"},
	{"accepted_prediction", "output"},
	{"rejected_prediction", "output"},
	{"audio_input", "input"},
	{"cache_creation_1h", "cache_creation"},
}

// Billable returns the count of key to charge at key's own rate: the stored count less its parts that rated reports as charged at a rate of their own, so no token is charged twice. A part without a rate stays in its whole. The stored counts are not changed.
//
// When the rated parts add up to more than the whole they cannot be inside it (a provider that reports them apart), and nothing is deducted.
func (t Tokens) Billable(key string, rated func(part string) bool) int64 {
	count := t[key]
	var deduct int64
	for _, p := range parts {
		if p.whole != key {
			continue
		}
		if n := t[p.part]; n > 0 && rated(p.part) {
			deduct += n
		}
	}
	if deduct > count {
		return count
	}
	return count - deduct
}

// PromptTokens returns the request's prompt length: input + cache_read + cache_creation. Context-length price tiers are chosen by it, since providers count cache reads and writes toward the threshold.
func (t Tokens) PromptTokens() int64 {
	return t["input"] + t["cache_read"] + t["cache_creation"]
}

// Add adds other into t in place. Useful for streaming chunks where each
// chunk emits a partial usage block.
func (t Tokens) Add(other Tokens) {
	for k, v := range other {
		t[k] += v
	}
}

// Sum returns the arithmetic total of all values. Used by legacy
// single-meter rate-limit callers that haven't migrated to typed meters.
//
// NOT a token total for billing/usage reporting: some keys are
// sub-breakdowns of a coarser one (e.g. reasoning ⊂ output), so Sum
// double-counts them. See Tokens for the total.
func (t Tokens) Sum() int64 {
	var s int64
	for _, v := range t {
		s += v
	}
	return s
}
