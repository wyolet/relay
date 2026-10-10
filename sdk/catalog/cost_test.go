package catalog

import (
	"math"
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
	// tier axis = prompt length; above the 200k bracket the whole input is
	// priced at the higher rate.
	cost, _, ok := b.CostBreakdown(usage.Tokens{"input": 1_000_000})
	if !ok {
		t.Fatal("expected priced binding")
	}
	if want := 6.0; cost != want {
		t.Fatalf("tiered cost = %v, want %v", cost, want)
	}
}

// Cache reads and writes count toward the prompt length that picks the tier, and the tier applies to every meter.
func TestCostBreakdown_TierByPromptLength(t *testing.T) {
	b := Binding{Pricing: []Rate{
		{Meter: "tokens.input", Unit: "per_million", Amount: 1},
		{Meter: "tokens.input", Unit: "per_million", Amount: 2, AboveTokens: 100_000},
		{Meter: "tokens.output", Unit: "per_million", Amount: 5},
		{Meter: "tokens.output", Unit: "per_million", Amount: 10, AboveTokens: 100_000},
		{Meter: "tokens.cache_read", Unit: "per_million", Amount: 0.1},
		{Meter: "tokens.cache_read", Unit: "per_million", Amount: 0.2, AboveTokens: 100_000},
		{Meter: "tokens.cache_creation", Unit: "per_million", Amount: 1.25},
		{Meter: "tokens.cache_creation", Unit: "per_million", Amount: 2.5, AboveTokens: 100_000},
	}}
	cases := []struct {
		name   string
		tokens usage.Tokens
		want   float64
	}{
		{"cache_read pushes the prompt over", usage.Tokens{"input": 1_000, "cache_read": 299_000, "output": 500}, 0.001*2 + 0.299*0.2 + 0.0005*10},
		{"prompt at the threshold picks upper", usage.Tokens{"input": 1_000, "cache_creation": 99_000, "output": 100}, 0.001*2 + 0.099*2.5 + 0.0001*10},
		{"cached prompt one below stays base", usage.Tokens{"input": 1_000, "cache_read": 98_999}, 0.001*1 + 0.098999*0.1},
		{"uncached prompt under stays base", usage.Tokens{"input": 99_999, "output": 100}, 0.099999*1 + 0.0001*5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cost, unpriced, ok := b.CostBreakdown(tc.tokens)
			if !ok || len(unpriced) != 0 || math.Abs(cost-tc.want) > 1e-12 {
				t.Fatalf("cost=%v unpriced=%v ok=%v, want %v", cost, unpriced, ok, tc.want)
			}
		})
	}
}

// 1-hour cache writes are counted inside cache_creation; with their own rate they are charged there and the rest of cache_creation at the 5-minute rate.
func TestBindingCost_CacheCreation1h(t *testing.T) {
	b := Binding{Pricing: []Rate{
		{Meter: "tokens.cache_creation", Unit: "per_million", Amount: 1.25},
		{Meter: "tokens.cache_creation_1h", Unit: "per_million", Amount: 2},
	}}
	tokens := usage.Tokens{"cache_creation": 4_000_000, "cache_creation_1h": 1_000_000}
	cost, unpriced, _ := b.CostBreakdown(tokens)
	if want := 3*1.25 + 1*2.0; math.Abs(cost-want) > 1e-9 || len(unpriced) != 0 {
		t.Fatalf("with a 1h rate: cost = %v unpriced = %v, want %v", cost, unpriced, want)
	}
	cost, _, _ = Binding{Pricing: b.Pricing[:1]}.CostBreakdown(tokens)
	if want := 4 * 1.25; math.Abs(cost-want) > 1e-9 {
		t.Fatalf("without a 1h rate: cost = %v, want %v", cost, want)
	}
}

// Reasoning is counted inside output; with a reasoning rate it is charged there and not again at the output rate.
func TestCostBreakdown_PartChargedOnce(t *testing.T) {
	b := Binding{Pricing: []Rate{
		{Meter: "tokens.output", Unit: "per_million", Amount: 1.2},
		{Meter: "tokens.reasoning", Unit: "per_million", Amount: 4},
	}}
	cost, _, _ := b.CostBreakdown(usage.Tokens{"output": 3_000_000, "reasoning": 2_500_000})
	if want := 0.5*1.2 + 2.5*4; math.Abs(cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
	cost, _, _ = Binding{Pricing: b.Pricing[:1]}.CostBreakdown(usage.Tokens{"output": 3_000_000, "reasoning": 2_500_000})
	if want := 3 * 1.2; math.Abs(cost-want) > 1e-9 {
		t.Fatalf("without a reasoning rate: cost = %v, want %v", cost, want)
	}
}

func TestCostBreakdownForServiceTier(t *testing.T) {
	b := Binding{Pricing: []Rate{
		{Meter: "tokens.input", Unit: "per_million", Amount: 4, ServiceTier: "priority"},
		{Meter: "tokens.input", Unit: "per_million", Amount: 2},
		{Meter: "tokens.output", Unit: "per_million", Amount: 8},
	}}
	tokens := usage.Tokens{"input": 1_000_000, "output": 100_000, "audio_input": 0}
	for serviceTier, want := range map[string]float64{"": 2.8, "priority": 4.8, "flex": 2.8} {
		cost, unpriced, ok := b.CostBreakdownForServiceTier(tokens, serviceTier)
		if !ok || len(unpriced) != 0 || math.Abs(cost-want) > 1e-9 {
			t.Fatalf("service tier %q: cost=%v unpriced=%v ok=%v, want %v", serviceTier, cost, unpriced, ok, want)
		}
	}
	if cost, _ := b.Cost(tokens); math.Abs(cost-2.8) > 1e-9 {
		t.Fatalf("Cost must price at base rates, got %v", cost)
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
