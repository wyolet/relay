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
// is the prompt length (usage.Tokens.PromptTokens, cached tokens included);
// the rate with the largest AboveTokens ≤ prompt length applies, and the
// WHOLE meter count bills at that tier (no marginal splitting) — the
// conventional context-length-tier model across providers.
//
// ok is false when nothing was priced: nil/disabled pricing, no tokens, or
// no token key matched a rate. Callers must treat !ok as "unpriced", never
// as a zero cost — a fabricated $0 is the silent-drop bug class. A genuine
// zero (priced meters with zero counts) returns ok=true with total 0.
//
// Each token is charged once. Some keys count tokens that are also inside input or output (usage.Tokens lists them); when the sheet has a rate for such a part, the part is charged there and left out of the whole's charge. So a breakdown entry is rate × usage.Tokens.Billable for its key, which for input and output can be less than rate × the stored count. The tier still comes from the stored counts.
//
// CostNanos prices at the base rates; CostNanosForServiceTier prices a response the upstream reports as served in a service tier.
func (p *Pricing) CostNanos(tokens usage.Tokens) (total int64, breakdown map[string]int64, ok bool) {
	return p.CostNanosForServiceTier(tokens, "")
}

// CostNanosForServiceTier is CostNanos with rates picked as in RateForServiceTier.
func (p *Pricing) CostNanosForServiceTier(tokens usage.Tokens, serviceTier string) (total int64, breakdown map[string]int64, ok bool) {
	if p == nil || !p.IsEnabled() || len(tokens) == 0 {
		return 0, nil, false
	}
	tier := int(tokens.PromptTokens())
	for key := range tokens {
		rate, found := p.rateForKey(key, tier, serviceTier)
		if !found {
			continue
		}
		count := p.billable(tokens, key, tier, serviceTier)
		var n int64
		switch rate.Unit {
		case UnitPerMillion:
			n = perMillionNanos(count, rateNanos(rate.Amount))
		case UnitPerUnit:
			n = count * rateNanos(rate.Amount)
		}
		if breakdown == nil {
			breakdown = make(map[string]int64, len(tokens))
		}
		breakdown[string(rate.Meter)] += n
		total += n
		ok = true
	}
	return total, breakdown, ok
}

// rateForKey returns the rate that charges a usage key at the given tier, or false when the sheet does not charge that key.
func (p *Pricing) rateForKey(key string, tier int, serviceTier string) (*Rate, bool) {
	meter, known := MeterForUsageKey(key)
	if !known {
		return nil, false
	}
	rate, found := p.RateForServiceTier(meter, tier, serviceTier)
	if !found || (rate.Unit != UnitPerMillion && rate.Unit != UnitPerUnit) {
		return nil, false
	}
	return rate, true
}

// billable is the count of a usage key this sheet charges at the key's own rate.
func (p *Pricing) billable(tokens usage.Tokens, key string, tier int, serviceTier string) int64 {
	return tokens.Billable(key, func(part string) bool {
		_, rated := p.rateForKey(part, tier, serviceTier)
		return rated
	})
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
