package catalog

import (
	"sort"

	"github.com/wyolet/relay/sdk/usage"
)

// Cost returns total cost in the rate sheet's currency and ok=false when the
// binding carries no pricing. Tier axis = prompt length, usage.Tokens.PromptTokens
// (matches app/pricing).
// It is the terse form of CostBreakdown, discarding the unpriced-meter list;
// callers rendering an estimate should prefer CostBreakdown so unpriced meters
// don't silently deflate the total.
func (b Binding) Cost(tokens usage.Tokens) (float64, bool) {
	return b.CostForServiceTier(tokens, "")
}

// CostForServiceTier is Cost for a response the upstream reports as served in
// serviceTier (the canonical Response.ServiceTier); see
// CostBreakdownForServiceTier.
func (b Binding) CostForServiceTier(tokens usage.Tokens, serviceTier string) (float64, bool) {
	cost, _, ok := b.CostBreakdownForServiceTier(tokens, serviceTier)
	return cost, ok
}

// CostBreakdown prices tokens against the binding's rate sheet and reports the
// meters that carried a non-zero count but produced no cost. cost is the total
// in the rate sheet's currency; unpriced lists the usage keys that went
// unpriced — a meter this binding doesn't price, or a key outside the catalog's
// meter vocabulary — sorted for stable output; ok is false only when the
// binding has no pricing at all (cost 0, unpriced nil). Tier axis = prompt
// length. Surface unpriced rather than presenting cost as complete: an unpriced
// meter is silently missing money otherwise, and a newly-priced meter type
// would quietly deflate every estimate that ignored it. Each token is charged
// once: a part with a rate of its own (see usage.Tokens) is left out of its
// whole's charge. A part without a rate of its own is billed inside its whole,
// so it is listed unpriced only when the whole is unpriced too or the part's
// tokens do not fit inside the whole's count. Prices at the base rates; see
// CostBreakdownForServiceTier.
func (b Binding) CostBreakdown(tokens usage.Tokens) (cost float64, unpriced []string, ok bool) {
	return b.CostBreakdownForServiceTier(tokens, "")
}

// CostBreakdownForServiceTier is CostBreakdown for a response served in
// serviceTier. For each meter a rate carrying that service tier wins over the
// base rate whenever one qualifies at the request's prompt length; a meter the
// tier does not price bills at the base rate. Matches app/pricing.
func (b Binding) CostBreakdownForServiceTier(tokens usage.Tokens, serviceTier string) (cost float64, unpriced []string, ok bool) {
	if len(b.Pricing) == 0 || len(tokens) == 0 {
		return 0, nil, false
	}
	tier := int(tokens.PromptTokens())
	hasRate := func(key string) bool {
		_, ok := b.rateForKey(key, tier, serviceTier)
		return ok
	}
	for key, count := range tokens {
		if count == 0 {
			continue
		}
		if meter, known := meterForUsageKey(key); known {
			if rate, rated := rateFor(b.Pricing, meter, tier, serviceTier); rated {
				count := tokens.Billable(key, hasRate)
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
		if billedInWhole(tokens, key, hasRate) {
			continue
		}
		unpriced = append(unpriced, key)
	}
	sort.Strings(unpriced)
	return cost, unpriced, true
}

// billedInWhole reports whether part, which has no rate of its own, was charged inside its whole: the whole is priced and its count holds every unrated part. Unrated parts that outgrow the whole were reported apart from it and went uncharged.
func billedInWhole(tokens usage.Tokens, part string, hasRate func(string) bool) bool {
	whole, isPart := usage.WholeOf(part)
	if !isPart || !hasRate(whole) {
		return false
	}
	var unrated int64
	for key, n := range tokens {
		if w, ok := usage.WholeOf(key); ok && w == whole && n > 0 && !hasRate(key) {
			unrated += n
		}
	}
	return tokens[whole] > 0 && unrated <= tokens[whole]
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

// rateForKey returns the rate that charges a usage key at the given tier, or false when the binding does not charge that key.
func (b Binding) rateForKey(key string, tier int, serviceTier string) (*Rate, bool) {
	meter, known := meterForUsageKey(key)
	if !known {
		return nil, false
	}
	rate, found := rateFor(b.Pricing, meter, tier, serviceTier)
	if !found || (rate.Unit != "per_million" && rate.Unit != "per_unit") {
		return nil, false
	}
	return rate, true
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
	case "cache_creation_1h":
		return "tokens.cache_creation_1h", true
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

func rateFor(rates []Rate, meter string, tokens int, serviceTier string) (*Rate, bool) {
	if serviceTier != "" {
		if r, ok := rateInServiceTier(rates, meter, tokens, serviceTier); ok {
			return r, true
		}
	}
	return rateInServiceTier(rates, meter, tokens, "")
}

func rateInServiceTier(rates []Rate, meter string, tokens int, serviceTier string) (*Rate, bool) {
	var best *Rate
	for i := range rates {
		r := &rates[i]
		if r.Meter != meter || r.ServiceTier != serviceTier {
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
