package pricing

import (
	"testing"

	"github.com/wyolet/relay/sdk/usage"
)

func TestCost_SimpleInputOutput(t *testing.T) {
	p := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 3.0},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 15.0},
			},
		},
	}
	got := p.Cost(usage.Tokens{"input": 1_000_000, "output": 100_000})
	want := 3.0 + 1.5
	if got != want {
		t.Fatalf("cost = %v, want %v", got, want)
	}
}

func TestCost_TierByInput(t *testing.T) {
	p := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 3.0, AboveTokens: 0},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 6.0, AboveTokens: 200_000},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 15.0, AboveTokens: 0},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 22.5, AboveTokens: 200_000},
			},
		},
	}
	// 250k input pushes us into the upper tier for both.
	got := p.Cost(usage.Tokens{"input": 250_000, "output": 50_000})
	want := 6.0*0.25 + 22.5*0.05
	if got != want {
		t.Fatalf("upper tier: got %v want %v", got, want)
	}

	// 100k input stays in the base tier.
	got = p.Cost(usage.Tokens{"input": 100_000, "output": 50_000})
	want = 3.0*0.1 + 15.0*0.05
	if got != want {
		t.Fatalf("base tier: got %v want %v", got, want)
	}
}

func TestCost_UnpricedMetersSkipped(t *testing.T) {
	p := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates:    []Rate{{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 3.0}},
		},
	}
	// audio_input not priced; should not panic, just skipped.
	got := p.Cost(usage.Tokens{"input": 1_000_000, "audio_input": 5000})
	want := 3.0
	if got != want {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCost_Disabled(t *testing.T) {
	disabled := false
	p := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Enabled:  &disabled,
			Rates:    []Rate{{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 3.0}},
		},
	}
	if got := p.Cost(usage.Tokens{"input": 1_000_000}); got != 0 {
		t.Fatalf("disabled pricing should return 0, got %v", got)
	}
}

func TestCost_NilOrEmpty(t *testing.T) {
	var p *Pricing
	if got := p.Cost(usage.Tokens{"input": 1000}); got != 0 {
		t.Fatalf("nil pricing should return 0, got %v", got)
	}
	p2 := &Pricing{Spec: Spec{Currency: "USD"}}
	if got := p2.Cost(nil); got != 0 {
		t.Fatalf("empty tokens should return 0, got %v", got)
	}
}
