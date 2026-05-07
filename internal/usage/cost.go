package usage

import "github.com/wyolet/relay/internal/catalog"

// Cost computes cost + currency from a Tokens map and an effective Pricing.
// Returns (0, "", false) if pricing is nil or unit is unrecognised.
// Unknown meters are skipped silently; metricUnpricedMeter is incremented so
// operators can detect "parser added a meter but no rate is configured" bugs.
func Cost(tokens Tokens, pricing *catalog.Pricing) (cost float64, currency string, ok bool) {
	if pricing == nil {
		return 0, "", false
	}
	var divisor float64
	switch pricing.Unit {
	case catalog.PricingUnitPerMillion:
		divisor = 1_000_000
	case catalog.PricingUnitPerThousand:
		divisor = 1_000
	case catalog.PricingUnitPerUnit:
		divisor = 1
	default:
		return 0, pricing.Currency, false
	}
	var sum float64
	for meter, count := range tokens {
		rate, present := pricing.Rates[meter]
		if !present {
			metricUnpricedMeter.WithLabelValues(meter).Inc()
			continue
		}
		sum += rate * float64(count) / divisor
	}
	return sum, pricing.Currency, true
}
