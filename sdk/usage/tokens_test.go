package usage

import (
	"slices"
	"testing"
)

func TestTokensBillable(t *testing.T) {
	rated := func(keys ...string) func(string) bool {
		return func(k string) bool { return slices.Contains(keys, k) }
	}
	tokens := Tokens{"input": 1000, "audio_input": 200, "cache_read": 300, "output": 500, "reasoning": 100, "audio_output": 50, "accepted_prediction": 20, "rejected_prediction": 10}
	cacheWrite := Tokens{"input": 10, "cache_creation": 900, "cache_creation_1h": 600}
	tests := []struct {
		name   string
		tokens Tokens
		key    string
		rated  func(string) bool
		want   int64
	}{
		{"no part rated", tokens, "output", rated(), 500},
		{"one part rated", tokens, "output", rated("reasoning"), 400},
		{"every output part rated", tokens, "output", rated("reasoning", "audio_output", "accepted_prediction", "rejected_prediction"), 320},
		{"audio input rated", tokens, "input", rated("audio_input"), 800},
		{"a part of output does not reduce input", tokens, "input", rated("reasoning"), 1000},
		{"cache_read is not a part of input", tokens, "input", rated("cache_read"), 1000},
		{"a part is billed whole", tokens, "reasoning", rated("reasoning"), 100},
		{"absent key", tokens, "cache_creation", rated(), 0},
		{"1h writes unrated stay in cache_creation", cacheWrite, "cache_creation", rated(), 900},
		{"1h writes rated leave the 5m writes", cacheWrite, "cache_creation", rated("cache_creation_1h"), 300},
		{"1h writes do not reduce input", cacheWrite, "input", rated("cache_creation_1h"), 10},
		{"parts equal to the whole", Tokens{"output": 100, "reasoning": 100}, "output", rated("reasoning"), 0},
		// A provider that reports a part outside its whole shows it by a part larger than the whole; deducting would erase the charge for the whole's own tokens.
		{"parts larger than the whole", Tokens{"output": 498, "reasoning": 95_305}, "output", rated("reasoning"), 498},
		{"negative part is not deducted", Tokens{"output": 10, "reasoning": -5}, "output", rated("reasoning"), 10},
		{"nil tokens", nil, "output", rated("reasoning"), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tokens.Billable(tc.key, tc.rated); got != tc.want {
				t.Errorf("Billable(%q) = %d, want %d", tc.key, got, tc.want)
			}
		})
	}
	if tokens["output"] != 500 || tokens["input"] != 1000 {
		t.Errorf("Billable changed the stored counts: %v", tokens)
	}
}

func TestWholeOf(t *testing.T) {
	for part, want := range map[string]string{
		"reasoning": "output", "audio_output": "output", "accepted_prediction": "output", "rejected_prediction": "output",
		"audio_input": "input", "cache_creation_1h": "cache_creation",
	} {
		if got, ok := WholeOf(part); !ok || got != want {
			t.Errorf("WholeOf(%q) = %q, %v, want %q, true", part, got, ok, want)
		}
	}
	for _, key := range []string{"output", "input", "cache_creation", "cache_read", "server_tool_use_input", "mystery", ""} {
		if got, ok := WholeOf(key); ok || got != "" {
			t.Errorf("WholeOf(%q) = %q, %v, want \"\", false", key, got, ok)
		}
	}
}

// audio_input is already inside input, and cache_creation_1h inside cache_creation, so neither adds to the prompt length.
func TestTokensPromptTokens(t *testing.T) {
	for _, tc := range []struct {
		tokens Tokens
		want   int64
	}{
		{Tokens{"input": 1000, "audio_input": 200, "cache_read": 300, "cache_creation": 50, "output": 500, "reasoning": 100}, 1350},
		{Tokens{"input": 10, "cache_creation": 900, "cache_creation_1h": 600}, 910},
		{Tokens{"output": 10}, 0},
		{nil, 0},
	} {
		if got := tc.tokens.PromptTokens(); got != tc.want {
			t.Errorf("%v.PromptTokens() = %d, want %d", tc.tokens, got, tc.want)
		}
	}
}

func TestTokensAddAndSum(t *testing.T) {
	total := Tokens{"input": 1, "output": 2}
	total.Add(Tokens{"input": 10, "cache_read": 5})
	total.Add(nil)
	if len(total) != 3 || total["input"] != 11 || total["output"] != 2 || total["cache_read"] != 5 {
		t.Errorf("Add = %v, want input 11, output 2, cache_read 5", total)
	}
	if got := total.Sum(); got != 18 {
		t.Errorf("Sum() = %d, want 18", got)
	}
	if got := Tokens(nil).Sum(); got != 0 {
		t.Errorf("nil Sum() = %d, want 0", got)
	}
}
