package pricing

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/sdk/usage"
)

// serviceTierSheet lists the tier rows first so a lookup that ignored
// ServiceTier would pick them up for base requests.
func serviceTierSheet() *Pricing {
	return &Pricing{
		Meta: meta.Metadata{Name: "tiered", Owner: meta.Owner{Kind: meta.OwnerHost, ID: "h1"}},
		Spec: Spec{
			Currency:       "USD",
			TargetModelIDs: []string{"m1"},
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 4, ServiceTier: "priority"},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 8, AboveTokens: 272_000, ServiceTier: "priority"},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 16, ServiceTier: "priority"},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 1, ServiceTier: "flex"},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 2},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 8},
				{Meter: MeterTokensCacheRead, Unit: UnitPerMillion, Amount: 0.5},
			},
		},
	}
}

func TestCostNanosForServiceTier(t *testing.T) {
	p := serviceTierSheet()
	cases := []struct {
		name        string
		serviceTier string
		tokens      usage.Tokens
		want        int64
	}{
		{"base", "", usage.Tokens{"input": 1_000_000, "output": 100_000}, 2_800_000_000},
		{"priority overrides both meters", "priority", usage.Tokens{"input": 200_000, "output": 100_000}, 2_400_000_000},
		{"priority ladder above threshold", "priority", usage.Tokens{"input": 300_000}, 2_400_000_000},
		{"priority ladder reached by cached prompt", "priority", usage.Tokens{"input": 100_000, "cache_read": 200_000}, 900_000_000},
		{"flex falls back per meter", "flex", usage.Tokens{"input": 1_000_000, "output": 100_000}, 1_800_000_000},
		{"flex cache_read bills at base", "flex", usage.Tokens{"input": 1_000_000, "cache_read": 200_000}, 1_100_000_000},
		{"unknown tier bills at base", "scale", usage.Tokens{"input": 1_000_000, "output": 100_000}, 2_800_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, ok := p.CostNanosForServiceTier(tc.tokens, tc.serviceTier)
			if !ok || got != tc.want {
				t.Fatalf("got %d (ok=%v), want %d", got, ok, tc.want)
			}
		})
	}
	if base, _, _ := p.CostNanos(usage.Tokens{"input": 1_000_000, "output": 100_000}); base != 2_800_000_000 {
		t.Fatalf("CostNanos must price at base rates, got %d", base)
	}
}

func TestRateFor_IgnoresServiceTierRates(t *testing.T) {
	r, ok := serviceTierSheet().RateFor(MeterTokensInput, 300_000)
	if !ok || r.Amount != 2 {
		t.Fatalf("RateFor = %+v, want the base input rate", r)
	}
}

func TestValidate_ServiceTierRates(t *testing.T) {
	p := serviceTierSheet()
	if err := p.Validate(); err != nil {
		t.Fatalf("base and tier rows on the same meter must validate: %v", err)
	}
	p.Spec.Rates = append(p.Spec.Rates, Rate{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 5, ServiceTier: "priority"})
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate tier row: err = %v", err)
	}
}
