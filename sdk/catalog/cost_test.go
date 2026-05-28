package catalog

import (
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

func TestCost_SimpleInputOutput(t *testing.T) {
	b := Binding{
		Pricing: []Rate{
			{Meter: "tokens.input", Unit: "per_million", Amount: 3.0},
			{Meter: "tokens.output", Unit: "per_million", Amount: 15.0},
		},
	}
	got, ok := b.Cost(usage.Tokens{"input": 1_000_000, "output": 100_000})
	if !ok {
		t.Fatal("expected priced binding")
	}
	want := 3.0 + 1.5
	if got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestCost_Unpriced(t *testing.T) {
	b := Binding{}
	if _, ok := b.Cost(usage.Tokens{"input": 100}); ok {
		t.Fatal("expected unpriced")
	}
}
