package adapterspecs

import (
	"os"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// tieredCacheSheet is a published small-model rate sheet (USD per million, second figure from a 100K-token prompt): input 0.10/0.50, output 0.50/2.50, cache_creation 0.125/0.625, cache_creation_1h 0.20/1.00, cache_read 0.01/0.05.
func tieredCacheSheet() *pricing.Pricing {
	var rates []pricing.Rate
	for _, r := range []struct {
		meter       pricing.Meter
		base, above float64
	}{
		{pricing.MeterTokensInput, 0.10, 0.50},
		{pricing.MeterTokensOutput, 0.50, 2.50},
		{pricing.MeterTokensCacheCreation, 0.125, 0.625},
		{pricing.MeterTokensCacheCreation1h, 0.20, 1.00},
		{pricing.MeterTokensCacheRead, 0.01, 0.05},
	} {
		rates = append(rates,
			pricing.Rate{Meter: r.meter, Unit: pricing.UnitPerMillion, Amount: r.base},
			pricing.Rate{Meter: r.meter, Unit: pricing.UnitPerMillion, Amount: r.above, AboveTokens: 100_000},
		)
	}
	return &pricing.Pricing{Spec: pricing.Spec{Currency: "USD", Rates: rates}}
}

func catalogRates(p *pricing.Pricing) []sdkcatalog.Rate {
	out := make([]sdkcatalog.Rate, len(p.Spec.Rates))
	for i, r := range p.Spec.Rates {
		out[i] = sdkcatalog.Rate{Meter: string(r.Meter), Unit: string(r.Unit), Amount: r.Amount, AboveTokens: r.AboveTokens, ServiceTier: r.ServiceTier}
	}
	return out
}

// An upstream body with 1-hour cache writes, read by the spec's token extractor the way post-flight reads it, prices at the hand-computed amount on both cost engines.
func TestCostNanos_AnthropicCacheWrite1hBody(t *testing.T) {
	tests := []struct {
		file string
		want int64
	}{
		// 1000 input, 4000 cache writes of which 1500 are 1h, 300 output; prompt 5000, base tier:
		// 1000×100 + 2500×125 + 1500×200 + 300×500
		{"testdata/anthropic-cache-write-1h.json", 862_500},
		// 2000 input, 80,000 cache reads, 30,000 cache writes of which 20,000 are 1h, 1000 output; prompt 112,000, upper tier:
		// 2000×500 + 80,000×50 + 10,000×625 + 20,000×1000 + 1000×2500
		{"testdata/anthropic-cache-write-1h.sse", 33_750_000},
	}
	spec := adapter.NewRegistry(Specs()...).Spec(adapters.Anthropic)
	sheet := tieredCacheSheet()
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			tokens := spec.ExtractTokens(body)
			if tokens["cache_creation_1h"] == 0 {
				t.Fatalf("no cache_creation_1h in %v", tokens)
			}
			if got, _, ok := sheet.CostNanos(tokens); !ok || got != tc.want {
				t.Fatalf("CostNanos = %d ok = %v, want %d (tokens %v)", got, ok, tc.want, tokens)
			}
			usd, ok := sdkcatalog.Binding{Pricing: catalogRates(sheet)}.Cost(tokens)
			if diff := usd*1e9 - float64(tc.want); !ok || diff > 1e-3 || diff < -1e-3 {
				t.Fatalf("SDK Cost = %.9f USD ok = %v, want %d nanos", usd, ok, tc.want)
			}
		})
	}
}
