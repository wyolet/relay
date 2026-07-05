package catalog

import (
	"sort"

	"github.com/wyolet/relay/sdk/usage"
)

// Cost returns total cost in the rate sheet's currency and ok=false when the
// binding carries no pricing. Tier axis = input tokens (matches app/pricing).
// It is the terse form of CostBreakdown, discarding the unpriced-meter list;
// callers rendering an estimate should prefer CostBreakdown so unpriced meters
// don't silently deflate the total.
func (b Binding) Cost(tokens usage.Tokens) (float64, bool) {
	cost, _, ok := b.CostBreakdown(tokens)
	return cost, ok
}

// CostBreakdown prices tokens against the binding's rate sheet and reports the
// meters that carried a non-zero count but produced no cost. cost is the total
// in the rate sheet's currency; unpriced lists the usage keys that went
// unpriced — a meter this binding doesn't price, or a key outside the catalog's
// meter vocabulary — sorted for stable output; ok is false only when the
// binding has no pricing at all (cost 0, unpriced nil). Tier axis = input
// tokens. Surface unpriced rather than presenting cost as complete: an unpriced
// meter is silently missing money otherwise, and a newly-priced meter type
// would quietly deflate every estimate that ignored it.
func (b Binding) CostBreakdown(tokens usage.Tokens) (cost float64, unpriced []string, ok bool) {
	if len(b.Pricing) == 0 || len(tokens) == 0 {
		return 0, nil, false
	}
	tier := int(tokens["input"])
	for key, count := range tokens {
		if count == 0 {
			continue
		}
		if meter, known := meterForUsageKey(key); known {
			if rate, rated := rateFor(b.Pricing, meter, tier); rated {
				switch rate.Unit {
				case "per_million":
					cost += float64(count) / 1_000_000 * rate.Amount
					continue
				case "per_unit":
					cost += float64(count) * rate.Amount
					continue
				}
				// unknown unit: fall through to unpriced rather than $0
			}
		}
		unpriced = append(unpriced, key)
	}
	sort.Strings(unpriced)
	return cost, unpriced, true
}

// Cost resolves ref to a binding and prices tokens against it — the one-call
// path for "given a model ref and accumulated token counts, what did it cost?"
// The catalog ships knowing every model's rate sheet, so a consumer holding
// only session token totals needs no local price table. unpriced carries the
// meters the model doesn't price (see Binding.CostBreakdown); ok is false when
// ref doesn't resolve or the binding carries no pricing.
func (ic *IndexedCatalog) Cost(ref string, tokens usage.Tokens) (cost float64, unpriced []string, ok bool) {
	b, _, err := ic.Resolve(ref)
	if err != nil {
		return 0, nil, false
	}
	return b.CostBreakdown(tokens)
}

func meterForUsageKey(k string) (string, bool) {
	switch k {
	case "input":
		return "tokens.input", true
	case "output":
		return "tokens.output", true
	case "cache_read":
		return "tokens.cache_read", true
	case "cache_creation":
		return "tokens.cache_creation", true
	case "reasoning":
		return "tokens.reasoning", true
	case "audio_input":
		return "tokens.audio_input", true
	case "audio_output":
		return "tokens.audio_output", true
	case "accepted_prediction":
		return "tokens.accepted_prediction", true
	case "rejected_prediction":
		return "tokens.rejected_prediction", true
	case "server_tool_use_input":
		return "tokens.server_tool_use_input", true
	case "server_tool_use_output":
		return "tokens.server_tool_use_output", true
	}
	return "", false
}

func rateFor(rates []Rate, meter string, tokens int) (*Rate, bool) {
	var best *Rate
	for i := range rates {
		r := &rates[i]
		if r.Meter != meter {
			continue
		}
		if tokens < r.AboveTokens {
			continue
		}
		if best == nil || r.AboveTokens > best.AboveTokens {
			best = r
		}
	}
	return best, best != nil
}
