package catalog

import (
	"strings"
	"testing"
)

func TestNorm(t *testing.T) {
	long := strings.Repeat("a", 62)
	tests := []struct {
		in, ref, prefix, suffix string
	}{
		{"gpt-4o", "gpt-4o", "gpt-4o", "gpt-4o"},
		{"GPT-5.5", "gpt-5-5", "gpt-5-5", "gpt-5-5"},
		{"openai/gpt-4o@azure", "openai-gpt-4o-azure", "openai-gpt-4o-azure", "openai-gpt-4o-azure"},
		{"deepseek-v3.1:671b-cloud", "deepseek-v3-1-671b-cloud", "deepseek-v3-1-671b-cloud", "deepseek-v3-1-671b-cloud"},
		// A wildcard alias is cut at "*": the part before keeps its closing separator, the part after its opening one.
		{"claude-5x[", "claude-5x", "claude-5x-", "claude-5x"},
		{"]-latest", "latest", "latest", "-latest"},
		{"--a__b--", "a-b", "a-b-", "-a-b"},
		{"münchen", "m-nchen", "m-nchen", "m-nchen"},
		{"", "", "", ""},
		{"@/@", "", "", ""},
		{long + "-b", long, long + "-b", long + "-b"},
		{long + "bcd", long + "b", long + "bcd", long + "bcd"},
	}
	for _, tc := range tests {
		if got := normRef(tc.in); got != tc.ref {
			t.Errorf("normRef(%q) = %q, want %q", tc.in, got, tc.ref)
		}
		if got := normPrefix(tc.in); got != tc.prefix {
			t.Errorf("normPrefix(%q) = %q, want %q", tc.in, got, tc.prefix)
		}
		if got := normSuffix(tc.in); got != tc.suffix {
			t.Errorf("normSuffix(%q) = %q, want %q", tc.in, got, tc.suffix)
		}
	}
}
