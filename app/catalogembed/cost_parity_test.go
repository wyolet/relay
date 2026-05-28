package catalogembed

import (
	"testing"

	"github.com/wyolet/relay/app/pricing"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/usage"
)

func TestBindingCost_ParityWithAppPricing(t *testing.T) {
	p := &pricing.Pricing{
		Spec: pricing.Spec{
			Currency: "USD",
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 3.0, AboveTokens: 0},
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 6.0, AboveTokens: 200_000},
				{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 15.0, AboveTokens: 0},
				{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 22.5, AboveTokens: 200_000},
			},
		},
	}
	b := sdkcatalog.Binding{Pricing: ratesFrom(p)}

	cases := []usage.Tokens{
		{"input": 1_000_000, "output": 100_000},
		{"input": 250_000, "output": 50_000},
		{"input": 100_000, "output": 50_000},
		{"input": 1_000_000, "audio_input": 5000},
	}
	for _, tok := range cases {
		want := p.Cost(tok)
		got, ok := b.Cost(tok)
		if !ok {
			t.Fatalf("binding not priced for %v", tok)
		}
		if got != want {
			t.Fatalf("tokens %v: sdk cost %v, app pricing cost %v", tok, got, want)
		}
	}
}

func TestBindingCost_UnpricedParity(t *testing.T) {
	var p *pricing.Pricing
	b := sdkcatalog.Binding{}
	if p.Cost(usage.Tokens{"input": 100}) != 0 {
		t.Fatal("nil pricing should be zero")
	}
	if _, ok := b.Cost(usage.Tokens{"input": 100}); ok {
		t.Fatal("empty binding pricing should be unpriced")
	}
}
