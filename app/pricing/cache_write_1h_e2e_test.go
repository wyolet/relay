package pricing

import (
	"os"
	"testing"

	"github.com/wyolet/relay/sdk/adapters/anthropic"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// An upstream body with 1-hour cache writes, read by the adapter the way post-flight reads it, prices at the hand-computed amount on both cost engines.
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
	sheet := tieredCacheSheet()
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			tokens := anthropic.ExtractTokens(body)
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
