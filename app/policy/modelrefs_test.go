package policy

import "testing"

// Canonicalisation is the same function the control API uses, so a
// document and an API write of the same grant store the same string.
func TestCanonicalizeModelRefs_SlugifiesAndDedupes(t *testing.T) {
	got, err := CanonicalizeModelRefs([]string{"OpenAI/GPT-4o", "openai/gpt-4o", "anthropic/claude-3.5"})
	if err != nil {
		t.Fatalf("CanonicalizeModelRefs: %v", err)
	}
	want := []string{"openai/gpt-4o", "anthropic/claude-3-5"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
