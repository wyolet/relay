package main

import (
	"testing"

	"github.com/wyolet/relay/app/adapters"
)

// Anthropic's clients retry a 529 overloaded_error as an overload; a 429 reads to them as their own rate limit.
func TestAnthropicSpecAnswersOverloadAs529(t *testing.T) {
	for _, s := range buildAdapterSpecs() {
		if s.Name != adapters.Anthropic {
			continue
		}
		if s.Overloaded == nil || s.Overloaded.Status != 529 {
			t.Fatalf("Overloaded = %+v, want status 529", s.Overloaded)
		}
		return
	}
	t.Fatal("no anthropic spec")
}
