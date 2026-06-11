package usagelog

import (
	"github.com/wyolet/relay/app/pricing"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// PricingLookup resolves a pricing id to the rate sheet. Satisfied by a
// closure over app/catalog's live snapshot at the composition root —
// snapshot map read, zero I/O.
type PricingLookup func(id string) (*pricing.Pricing, bool)

// Pricer maps a request's stamped pricing identity + extracted token counts
// to the cost fields stamped on the usage Event. Cost is computed exactly
// once, here at emit time, from the rate sheet currently in the catalog
// snapshot — what a request cost is a historical fact, so readers never
// recompute it against (mutable) pricing config. Nil-safe: a nil Pricer
// (tests, partial wiring) prices nothing, leaving events unpriced.
type Pricer struct {
	lookup PricingLookup
}

// NewPricer constructs a Pricer over the given lookup.
func NewPricer(lookup PricingLookup) *Pricer { return &Pricer{lookup: lookup} }

// Price returns the total nano-USD cost + per-meter breakdown for the
// tokens under pricingID. ok=false means unpriced — no pricing id stamped,
// the rate sheet is gone from the snapshot, or no token key matched a rate.
// Unpriced is never reported as a zero cost.
func (p *Pricer) Price(pricingID string, tokens sdkusage.Tokens) (nanos int64, breakdown map[string]int64, ok bool) {
	if p == nil || p.lookup == nil || pricingID == "" || len(tokens) == 0 {
		return 0, nil, false
	}
	pr, found := p.lookup(pricingID)
	if !found {
		return 0, nil, false
	}
	return pr.CostNanos(tokens)
}
