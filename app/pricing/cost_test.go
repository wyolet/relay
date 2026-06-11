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

func TestCostNanos_RateTable(t *testing.T) {
	// The claude-opus-4-7 sheet shape: 4 meters, fractional per-million
	// amounts (6.25, 0.5) that must stay exact in integer nanos.
	opus := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 5},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 25},
				{Meter: MeterTokensCacheCreation, Unit: UnitPerMillion, Amount: 6.25},
				{Meter: MeterTokensCacheRead, Unit: UnitPerMillion, Amount: 0.5},
			},
		},
	}
	tiered := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 3},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 6, AboveTokens: 200_000},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 15},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 22.5, AboveTokens: 200_000},
			},
		},
	}
	perUnit := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates:    []Rate{{Meter: MeterTokensInput, Unit: UnitPerUnit, Amount: 0.001}},
		},
	}

	tests := []struct {
		name     string
		p        *Pricing
		tokens   usage.Tokens
		want     int64
		wantBkdn map[string]int64
	}{
		{
			name:   "each meter, fractional amounts exact",
			p:      opus,
			tokens: usage.Tokens{"input": 1000, "output": 200, "cache_creation": 400, "cache_read": 8000},
			// 1000×5000 + 200×25000 + 400×6250 + 8000×500 nanos
			want: 5_000_000 + 5_000_000 + 2_500_000 + 4_000_000,
			wantBkdn: map[string]int64{
				"tokens.input":          5_000_000,
				"tokens.output":         5_000_000,
				"tokens.cache_creation": 2_500_000,
				"tokens.cache_read":     4_000_000,
			},
		},
		{
			name: "sub-nano remainder truncates, still priced",
			p: &Pricing{Spec: Spec{Currency: "USD", Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 0.0005}, // 0.5 nanos/token
			}}},
			tokens:   usage.Tokens{"input": 3}, // 1.5 nanos → floor 1
			want:     1,
			wantBkdn: map[string]int64{"tokens.input": 1},
		},
		{
			name:   "base tier below AboveTokens",
			p:      tiered,
			tokens: usage.Tokens{"input": 199_999, "output": 100},
			want:   199_999*3_000 + 100*15_000,
			wantBkdn: map[string]int64{
				"tokens.input":  199_999 * 3_000,
				"tokens.output": 100 * 15_000,
			},
		},
		{
			name:   "tier boundary is inclusive — input == AboveTokens picks upper",
			p:      tiered,
			tokens: usage.Tokens{"input": 200_000, "output": 100},
			// upper tier applies to BOTH meters (tier axis = input).
			want: 200_000*6_000 + 100*22_500,
			wantBkdn: map[string]int64{
				"tokens.input":  200_000 * 6_000,
				"tokens.output": 100 * 22_500,
			},
		},
		{
			name:     "per_unit",
			p:        perUnit,
			tokens:   usage.Tokens{"input": 3},
			want:     3 * 1_000_000,
			wantBkdn: map[string]int64{"tokens.input": 3_000_000},
		},
		{
			name:   "overflow sanity — huge batch at a huge rate",
			p:      &Pricing{Spec: Spec{Currency: "USD", Rates: []Rate{{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 1000}}}},
			tokens: usage.Tokens{"input": 10_000_000}, // $10k
			want:   10_000_000 * 1_000_000,
			wantBkdn: map[string]int64{
				"tokens.input": 10_000_000 * 1_000_000,
			},
		},
		{
			name:     "zero counts on priced meters → honest $0, still priced",
			p:        opus,
			tokens:   usage.Tokens{"input": 0},
			want:     0,
			wantBkdn: map[string]int64{"tokens.input": 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, bkdn, ok := tc.p.CostNanos(tc.tokens)
			if !ok {
				t.Fatalf("ok=false, want priced")
			}
			if got != tc.want {
				t.Fatalf("total = %d, want %d", got, tc.want)
			}
			if len(bkdn) != len(tc.wantBkdn) {
				t.Fatalf("breakdown = %+v, want %+v", bkdn, tc.wantBkdn)
			}
			for k, v := range tc.wantBkdn {
				if bkdn[k] != v {
					t.Fatalf("breakdown[%s] = %d, want %d (full: %+v)", k, bkdn[k], v, bkdn)
				}
			}
		})
	}
}

func TestCostNanos_UnpricedNeverZero(t *testing.T) {
	p := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates:    []Rate{{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 3}},
		},
	}

	// nil pricing, empty tokens, disabled sheet, no key matching a rate —
	// all must come back ok=false, never a fabricated priced $0.
	var nilP *Pricing
	if _, _, ok := nilP.CostNanos(usage.Tokens{"input": 5}); ok {
		t.Fatal("nil pricing: want unpriced")
	}
	if _, _, ok := p.CostNanos(nil); ok {
		t.Fatal("no tokens: want unpriced")
	}
	disabled := false
	pd := &Pricing{Spec: Spec{Currency: "USD", Enabled: &disabled, Rates: p.Spec.Rates}}
	if _, _, ok := pd.CostNanos(usage.Tokens{"input": 5}); ok {
		t.Fatal("disabled pricing: want unpriced")
	}
	if _, _, ok := p.CostNanos(usage.Tokens{"output": 100}); ok {
		t.Fatal("no rate for any token key: want unpriced")
	}

	// Mixed: unmatched keys skip, matched keys price.
	total, bkdn, ok := p.CostNanos(usage.Tokens{"input": 1_000_000, "output": 999})
	if !ok || total != 3_000_000_000 || len(bkdn) != 1 {
		t.Fatalf("mixed: ok=%v total=%d bkdn=%+v", ok, total, bkdn)
	}
}

func TestCostNanos_MatchesFloatCost(t *testing.T) {
	p := &Pricing{
		Spec: Spec{
			Currency: "USD",
			Rates: []Rate{
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 5},
				{Meter: MeterTokensOutput, Unit: UnitPerMillion, Amount: 25},
				{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 2.5, AboveTokens: 1_000_000},
			},
		},
	}
	for _, tokens := range []usage.Tokens{
		{"input": 123_456, "output": 7_890},
		{"input": 1_000_000, "output": 1},
		{"input": 2_500_000},
	} {
		nanos, _, ok := p.CostNanos(tokens)
		if !ok {
			t.Fatalf("tokens %+v: unpriced", tokens)
		}
		f := p.Cost(tokens)
		diff := float64(nanos)/1e9 - f
		if diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("tokens %+v: nanos %d vs float %v (diff %v)", tokens, nanos, f, diff)
		}
	}
}
