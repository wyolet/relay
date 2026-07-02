package catalog

import (
	"strings"
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

func TestCostBreakdown_ReportsUnpricedMeters(t *testing.T) {
	b := Binding{
		Pricing: []Rate{
			{Meter: "tokens.input", Unit: "per_million", Amount: 3.0},
			{Meter: "tokens.output", Unit: "per_million", Amount: 15.0},
		},
	}
	// reasoning is a known meter this binding doesn't price; "mystery" is
	// outside the catalog vocabulary entirely. Both must surface, and neither
	// may contribute to cost.
	cost, unpriced, ok := b.CostBreakdown(usage.Tokens{
		"input":     1_000_000,
		"output":    100_000,
		"reasoning": 50_000,
		"mystery":   9,
	})
	if !ok {
		t.Fatal("expected priced binding")
	}
	if want := 3.0 + 1.5; cost != want {
		t.Fatalf("cost = %v, want %v (unpriced meters must not add)", cost, want)
	}
	if got := strings.Join(unpriced, ","); got != "mystery,reasoning" {
		t.Fatalf("unpriced = %q, want %q", got, "mystery,reasoning")
	}
}

func TestCostBreakdown_ZeroCountMetersNotUnpriced(t *testing.T) {
	b := Binding{Pricing: []Rate{{Meter: "tokens.input", Unit: "per_million", Amount: 3.0}}}
	_, unpriced, ok := b.CostBreakdown(usage.Tokens{"input": 1_000_000, "reasoning": 0})
	if !ok {
		t.Fatal("expected priced binding")
	}
	if len(unpriced) != 0 {
		t.Fatalf("zero-count meter should not be reported unpriced, got %v", unpriced)
	}
}

func TestCostBreakdown_TieredPricing(t *testing.T) {
	b := Binding{
		Pricing: []Rate{
			{Meter: "tokens.input", Unit: "per_million", Amount: 3.0, AboveTokens: 0},
			{Meter: "tokens.input", Unit: "per_million", Amount: 6.0, AboveTokens: 200_000},
		},
	}
	// tier axis = input count; above the 200k bracket the whole input is
	// priced at the higher rate.
	cost, _, ok := b.CostBreakdown(usage.Tokens{"input": 1_000_000})
	if !ok {
		t.Fatal("expected priced binding")
	}
	if want := 6.0; cost != want {
		t.Fatalf("tiered cost = %v, want %v", cost, want)
	}
}

func TestIndexedCatalog_Cost(t *testing.T) {
	ic, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	// Any Anthropic Claude binding prices input+output+cache in the shipped
	// catalog; resolve one and confirm the one-call path prices it.
	cost, _, ok := ic.Cost("claude-opus-4-5", usage.Tokens{"input": 1_000_000})
	if !ok || cost <= 0 {
		t.Fatalf("expected priced resolve, got cost=%v ok=%v", cost, ok)
	}
	if _, _, ok := ic.Cost("no-such-model-xyz", usage.Tokens{"input": 1}); ok {
		t.Fatal("unknown ref should not resolve to a price")
	}
}
