package pricing

import (
	"math"

	"github.com/wyolet/relay/sdk/usage"
)

// CostNanos computes the total cost in integer nano-USD (1 USD = 1e9 nanos)
// plus a per-meter breakdown keyed by Meter string. Integer money because
// the result is stamped onto immutable usage events — float drift across
// sums/aggregates is not acceptable for a billing-adjacent record, and
// per-request magnitudes fit int64 with room to spare (~9.2e9 USD).
//
// Tier semantics match Cost (and sdk/catalog.Binding.Cost): the tier axis
// is the input token count; the rate with the largest AboveTokens ≤ input
// applies, and the WHOLE meter count bills at that tier (no marginal
// splitting) — the conventional context-length-tier model across providers.
//
// ok is false when nothing was priced: nil/disabled pricing, no tokens, or
// no token key matched a rate. Callers must treat !ok as "unpriced", never
// as a zero cost — a fabricated $0 is the silent-drop bug class. A genuine
// zero (priced meters with zero counts) returns ok=true with total 0.
func (p *Pricing) CostNanos(tokens usage.Tokens) (total int64, breakdown map[string]int64, ok bool) {
	if p == nil || !p.IsEnabled() || len(tokens) == 0 {
		return 0, nil, false
	}
	tier := int(tokens["input"])
	for key, count := range tokens {
		meter, known := MeterForUsageKey(key)
		if !known {
			continue
		}
		rate, found := p.RateFor(meter, tier)
		if !found {
			continue
		}
		var n int64
		switch rate.Unit {
		case UnitPerMillion:
			n = perMillionNanos(count, rateNanos(rate.Amount))
		case UnitPerUnit:
			n = count * rateNanos(rate.Amount)
		default:
			continue
		}
		if breakdown == nil {
			breakdown = make(map[string]int64, len(tokens))
		}
		breakdown[string(meter)] += n
		total += n
		ok = true
	}
	return total, breakdown, ok
}

// rateNanos converts a float rate Amount to integer nano-USD once, so all
// downstream arithmetic is exact integer math. Catalog amounts carry ≤6
// decimal places, which 1e9 scaling represents exactly.
func rateNanos(amount float64) int64 {
	return int64(math.Round(amount * 1e9))
}

// perMillionNanos is count × rateN / 1e6 without intermediate overflow:
// rateN splits into its millions quotient and remainder so the largest
// intermediate is count × 999_999 — safe for any plausible token count.
// The sub-nano remainder truncates (floor); at most 1 nano-USD per meter.
func perMillionNanos(count, rateN int64) int64 {
	q, r := rateN/1_000_000, rateN%1_000_000
	return count*q + count*r/1_000_000
}
