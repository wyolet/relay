package pricing

import (
	"maps"
	"math/rand/v2"
	"testing"

	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/usage"
)

// tieredCacheSheet is a published small-model rate sheet (USD per million, second figure from a 100K-token prompt): input 0.10/0.50, output 0.50/2.50, cache_creation 0.125/0.625, cache_creation_1h 0.20/1.00, cache_read 0.01/0.05. Nanos per token: input 100/500, output 500/2500, cache_creation 125/625, cache_creation_1h 200/1000, cache_read 10/50.
func tieredCacheSheet() *Pricing {
	rate := func(m Meter, base, above float64) []Rate {
		return []Rate{
			{Meter: m, Unit: UnitPerMillion, Amount: base},
			{Meter: m, Unit: UnitPerMillion, Amount: above, AboveTokens: 100_000},
		}
	}
	var rates []Rate
	rates = append(rates, rate(MeterTokensInput, 0.10, 0.50)...)
	rates = append(rates, rate(MeterTokensOutput, 0.50, 2.50)...)
	rates = append(rates, rate(MeterTokensCacheCreation, 0.125, 0.625)...)
	rates = append(rates, rate(MeterTokensCacheCreation1h, 0.20, 1.00)...)
	rates = append(rates, rate(MeterTokensCacheRead, 0.01, 0.05)...)
	return &Pricing{Spec: Spec{Currency: "USD", Rates: rates}}
}

// withoutMeter returns p with every rate for meter removed.
func withoutMeter(p *Pricing, meter Meter) *Pricing {
	var rates []Rate
	for _, r := range p.Spec.Rates {
		if r.Meter != meter {
			rates = append(rates, r)
		}
	}
	return &Pricing{Spec: Spec{Currency: p.Spec.Currency, Rates: rates}}
}

type cacheWriteCase struct {
	name     string
	p        *Pricing
	tokens   usage.Tokens
	want     int64
	wantBkdn map[string]int64
}

func cacheWriteCases() []cacheWriteCase {
	sheet := tieredCacheSheet()
	no1h := withoutMeter(sheet, MeterTokensCacheCreation1h)
	return []cacheWriteCase{
		{
			name:   "no cache",
			p:      sheet,
			tokens: usage.Tokens{"input": 2_000, "output": 500},
			// 2000×100 + 500×500
			want:     450_000,
			wantBkdn: map[string]int64{"tokens.input": 200_000, "tokens.output": 250_000},
		},
		{
			name:   "5m writes only",
			p:      sheet,
			tokens: usage.Tokens{"input": 1_000, "cache_creation": 4_000, "output": 300},
			// 1000×100 + 4000×125 + 300×500
			want:     750_000,
			wantBkdn: map[string]int64{"tokens.input": 100_000, "tokens.cache_creation": 500_000, "tokens.output": 150_000},
		},
		{
			name:   "1h writes only",
			p:      sheet,
			tokens: usage.Tokens{"input": 1_000, "cache_creation": 4_000, "cache_creation_1h": 4_000, "output": 300},
			// 1000×100 + (4000−4000)×125 + 4000×200 + 300×500
			want:     1_050_000,
			wantBkdn: map[string]int64{"tokens.input": 100_000, "tokens.cache_creation": 0, "tokens.cache_creation_1h": 800_000, "tokens.output": 150_000},
		},
		{
			name:   "mixed 5m and 1h writes",
			p:      sheet,
			tokens: usage.Tokens{"input": 1_000, "cache_creation": 4_000, "cache_creation_1h": 1_500, "output": 300},
			// 1000×100 + (4000−1500)×125 + 1500×200 + 300×500
			want:     862_500,
			wantBkdn: map[string]int64{"tokens.input": 100_000, "tokens.cache_creation": 312_500, "tokens.cache_creation_1h": 300_000, "tokens.output": 150_000},
		},
		{
			name:   "sheet without a 1h rate bills every write at the 5m rate",
			p:      no1h,
			tokens: usage.Tokens{"input": 1_000, "cache_creation": 4_000, "cache_creation_1h": 1_500, "output": 300},
			// 1000×100 + 4000×125 + 300×500
			want:     750_000,
			wantBkdn: map[string]int64{"tokens.input": 100_000, "tokens.cache_creation": 500_000, "tokens.output": 150_000},
		},
		{
			name:   "cache_read leaves the prompt one under 100K",
			p:      sheet,
			tokens: usage.Tokens{"input": 1_000, "cache_read": 98_999, "output": 200},
			// prompt 99,999 → base: 1000×100 + 98,999×10 + 200×500
			want:     1_189_990,
			wantBkdn: map[string]int64{"tokens.input": 100_000, "tokens.cache_read": 989_990, "tokens.output": 100_000},
		},
		{
			name:   "cache_read brings the prompt to exactly 100K",
			p:      sheet,
			tokens: usage.Tokens{"input": 1_000, "cache_read": 99_000, "output": 200},
			// prompt 100,000 → upper: 1000×500 + 99,000×50 + 200×2500
			want:     5_950_000,
			wantBkdn: map[string]int64{"tokens.input": 500_000, "tokens.cache_read": 4_950_000, "tokens.output": 500_000},
		},
		{
			name:   "cache_read takes the prompt over 100K",
			p:      sheet,
			tokens: usage.Tokens{"input": 1_000, "cache_read": 150_000, "output": 200},
			// prompt 151,000 → upper: 1000×500 + 150,000×50 + 200×2500
			want:     8_500_000,
			wantBkdn: map[string]int64{"tokens.input": 500_000, "tokens.cache_read": 7_500_000, "tokens.output": 500_000},
		},
		{
			name:   "1h writes on a prompt over 100K",
			p:      sheet,
			tokens: usage.Tokens{"input": 2_000, "cache_read": 80_000, "cache_creation": 30_000, "cache_creation_1h": 20_000, "output": 1_000},
			// prompt 2000+80,000+30,000 = 112,000 → upper: 2000×500 + 80,000×50 + (30,000−20,000)×625 + 20,000×1000 + 1000×2500
			want: 33_750_000,
			wantBkdn: map[string]int64{
				"tokens.input":             1_000_000,
				"tokens.cache_read":        4_000_000,
				"tokens.cache_creation":    6_250_000,
				"tokens.cache_creation_1h": 20_000_000,
				"tokens.output":            2_500_000,
			},
		},
	}
}

func TestCostNanos_CacheCreation1h(t *testing.T) {
	for _, tc := range cacheWriteCases() {
		t.Run(tc.name, func(t *testing.T) {
			total, bkdn, ok := tc.p.CostNanos(tc.tokens)
			if !ok || total != tc.want {
				t.Fatalf("total = %d ok = %v, want %d", total, ok, tc.want)
			}
			if !maps.Equal(bkdn, tc.wantBkdn) {
				t.Fatalf("breakdown = %v, want %v", bkdn, tc.wantBkdn)
			}
		})
	}
}

// randomTokens returns a token map whose parts never exceed their wholes, as a provider reports them.
func randomTokens(r *rand.Rand) usage.Tokens {
	n := func(limit int64) int64 {
		if r.IntN(4) == 0 {
			return 0
		}
		return r.Int64N(limit + 1)
	}
	t := usage.Tokens{
		"input":          n(200_000),
		"cache_read":     n(200_000),
		"cache_creation": n(100_000),
		"output":         n(50_000),
	}
	t["cache_creation_1h"] = n(t["cache_creation"])
	t["audio_input"] = n(t["input"])
	t["reasoning"] = n(t["output"])
	t["audio_output"] = n(t["output"] - t["reasoning"])
	for k, v := range t {
		if v == 0 && r.IntN(2) == 0 {
			delete(t, k)
		}
	}
	return t
}

func TestCostNanos_CacheCreation1hInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	sheet := tieredCacheSheet()
	no1h := withoutMeter(sheet, MeterTokensCacheCreation1h)
	// 1h rows equal to the 5m rows, tier for tier.
	same := withoutMeter(sheet, MeterTokensCacheCreation1h)
	for _, rt := range no1h.Spec.Rates {
		if rt.Meter == MeterTokensCacheCreation {
			rt.Meter = MeterTokensCacheCreation1h
			same.Spec.Rates = append(same.Spec.Rates, rt)
		}
	}
	// One nano per token on every meter, so the total is the number of tokens charged.
	var perToken []Rate
	for _, m := range []Meter{MeterTokensInput, MeterTokensOutput, MeterTokensCacheRead, MeterTokensCacheCreation, MeterTokensCacheCreation1h, MeterTokensReasoning, MeterTokensAudioInput, MeterTokensAudioOutput} {
		perToken = append(perToken, Rate{Meter: m, Unit: UnitPerUnit, Amount: 1e-9})
	}
	countSheet := &Pricing{Spec: Spec{Currency: "USD", Rates: perToken}}

	for i := range 2_000 {
		tokens := randomTokens(r)
		total, bkdn, _ := sheet.CostNanos(tokens)
		var sum int64
		for _, v := range bkdn {
			sum += v
		}
		if sum != total {
			t.Fatalf("#%d %v: breakdown sums to %d, total %d", i, tokens, sum, total)
		}

		charged, _, _ := countSheet.CostNanos(tokens)
		if want := tokens["input"] + tokens["cache_read"] + tokens["cache_creation"] + tokens["output"]; charged != want {
			t.Fatalf("#%d %v: %d tokens charged, want %d", i, tokens, charged, want)
		}

		stripped := maps.Clone(tokens)
		delete(stripped, "cache_creation_1h")
		got, _, _ := no1h.CostNanos(tokens)
		want, _, _ := no1h.CostNanos(stripped)
		if got != want {
			t.Fatalf("#%d %v: sheet without a 1h rate charged %d, %d without the 1h count", i, tokens, got, want)
		}

		// Exact here because every rate is a whole number of nanos per token, so no per-meter flooring.
		got, _, _ = same.CostNanos(tokens)
		want, _, _ = no1h.CostNanos(tokens)
		if got != want {
			t.Fatalf("#%d %v: 1h rate equal to the 5m rate charged %d, want %d", i, tokens, got, want)
		}
	}
}

func catalogRates(p *Pricing) []sdkcatalog.Rate {
	out := make([]sdkcatalog.Rate, len(p.Spec.Rates))
	for i, r := range p.Spec.Rates {
		out[i] = sdkcatalog.Rate{Meter: string(r.Meter), Unit: string(r.Unit), Amount: r.Amount, AboveTokens: r.AboveTokens, ServiceTier: r.ServiceTier}
	}
	return out
}

// The server bills in integer nanos and the SDK estimates in float USD from the same sheet; they must agree.
func TestCostNanos_AgreesWithSDKCatalog(t *testing.T) {
	sheet := tieredCacheSheet()
	priority := tieredCacheSheet()
	priority.Spec.Rates = append(priority.Spec.Rates,
		Rate{Meter: MeterTokensInput, Unit: UnitPerMillion, Amount: 0.2, ServiceTier: "priority"},
		Rate{Meter: MeterTokensCacheCreation1h, Unit: UnitPerMillion, Amount: 0.4, ServiceTier: "priority"},
	)
	sheets := []*Pricing{sheet, withoutMeter(sheet, MeterTokensCacheCreation1h), priority}

	var tokenMaps []usage.Tokens
	for _, tc := range cacheWriteCases() {
		tokenMaps = append(tokenMaps, tc.tokens)
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 1_000 {
		tokenMaps = append(tokenMaps, randomTokens(r))
	}

	for _, p := range sheets {
		b := sdkcatalog.Binding{Pricing: catalogRates(p)}
		for _, serviceTier := range []string{"", "priority"} {
			for _, tokens := range tokenMaps {
				nanos, _, _ := p.CostNanosForServiceTier(tokens, serviceTier)
				usd, _, _ := b.CostBreakdownForServiceTier(tokens, serviceTier)
				// Float rounding only: every rate here is a whole number of nanos per token, so the nano path does not floor.
				if diff := usd*1e9 - float64(nanos); diff > 1e-3 || diff < -1e-3 {
					t.Fatalf("tier %q %v: server %d nanos, SDK %.6f nanos", serviceTier, tokens, nanos, usd*1e9)
				}
			}
		}
	}
}
