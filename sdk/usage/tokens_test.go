package usage

import "testing"

func TestTokensBillable(t *testing.T) {
	rated := func(keys ...string) func(string) bool {
		return func(k string) bool {
			for _, r := range keys {
				if r == k {
					return true
				}
			}
			return false
		}
	}
	tokens := Tokens{"input": 1000, "audio_input": 200, "cache_read": 300, "output": 500, "reasoning": 100, "audio_output": 50, "accepted_prediction": 20, "rejected_prediction": 10}
	tests := []struct {
		name  string
		key   string
		rated func(string) bool
		want  int64
	}{
		{"no part rated", "output", rated(), 500},
		{"one part rated", "output", rated("reasoning"), 400},
		{"every output part rated", "output", rated("reasoning", "audio_output", "accepted_prediction", "rejected_prediction"), 320},
		{"audio input rated", "input", rated("audio_input"), 800},
		{"a part of output does not reduce input", "input", rated("reasoning"), 1000},
		{"cache_read is not a part of input", "input", rated("cache_read"), 1000},
		{"a part is billed whole", "reasoning", rated("reasoning"), 100},
		{"absent key", "cache_creation", rated(), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokens.Billable(tc.key, tc.rated); got != tc.want {
				t.Errorf("Billable(%q) = %d, want %d", tc.key, got, tc.want)
			}
		})
	}
	if tokens["output"] != 500 || tokens["input"] != 1000 {
		t.Errorf("Billable changed the stored counts: %v", tokens)
	}
}

// audio_input is already inside input, so it adds nothing to the prompt length.
func TestTokensPromptTokens(t *testing.T) {
	tokens := Tokens{"input": 1000, "audio_input": 200, "cache_read": 300, "cache_creation": 50, "output": 500, "reasoning": 100}
	if got := tokens.PromptTokens(); got != 1350 {
		t.Errorf("PromptTokens() = %d, want 1350", got)
	}
	if got := (Tokens{"output": 10}).PromptTokens(); got != 0 {
		t.Errorf("PromptTokens() without prompt keys = %d, want 0", got)
	}
}

// A provider that reports a part outside its whole shows it by a part larger than the whole; deducting would erase the charge for the whole's own tokens.
func TestTokensBillable_PartsLargerThanWhole(t *testing.T) {
	tokens := Tokens{"output": 498, "reasoning": 95_305}
	if got := tokens.Billable("output", func(string) bool { return true }); got != 498 {
		t.Errorf("Billable(output) = %d, want 498", got)
	}
}
