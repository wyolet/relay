package catalog

import "github.com/wyolet/relay/sdk/usage"

// Cost returns total cost in the rate sheet's currency and ok=false when the
// binding carries no pricing. Tier axis = input tokens (matches app/pricing).
func (b Binding) Cost(tokens usage.Tokens) (float64, bool) {
	if len(b.Pricing) == 0 || len(tokens) == 0 {
		return 0, false
	}
	tier := int(tokens["input"])
	var total float64
	for key, count := range tokens {
		meter, ok := meterForUsageKey(key)
		if !ok {
			continue
		}
		rate, ok := rateFor(b.Pricing, meter, tier)
		if !ok {
			continue
		}
		switch rate.Unit {
		case "per_million":
			total += float64(count) / 1_000_000 * rate.Amount
		case "per_unit":
			total += float64(count) * rate.Amount
		}
	}
	return total, true
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
