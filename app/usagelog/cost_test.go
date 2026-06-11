package usagelog

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/pkg/lifecycle"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// stubTranslator hands buildEvent fixed canonical usage without pulling a
// real vendor adapter into the test binary.
type stubTranslator struct{ tokens sdkusage.Tokens }

func (stubTranslator) ParseRequest([]byte) (*v1.Request, error)     { return nil, nil }
func (stubTranslator) SerializeRequest(*v1.Request) ([]byte, error) { return nil, nil }
func (s stubTranslator) ParseResponse([]byte) (*v1.Response, error) {
	return &v1.Response{Usage: s.tokens}, nil
}
func (stubTranslator) SerializeResponse(*v1.Response, *v1.Request) ([]byte, error) {
	return nil, nil
}
func (stubTranslator) NewToCanonicalStream() func([]byte) ([]byte, error)   { return nil }
func (stubTranslator) NewFromCanonicalStream() func([]byte) ([]byte, error) { return nil }

func testPricer(sheets map[string]*pricing.Pricing) *Pricer {
	return NewPricer(func(id string) (*pricing.Pricing, bool) {
		p, ok := sheets[id]
		return p, ok
	})
}

func testSheet() *pricing.Pricing {
	return &pricing.Pricing{
		Meta: meta.Metadata{ID: "pr-1", Name: "anthropic-opus"},
		Spec: pricing.Spec{
			Currency: "USD",
			Rates: []pricing.Rate{
				{Meter: pricing.MeterTokensInput, Unit: pricing.UnitPerMillion, Amount: 5},
				{Meter: pricing.MeterTokensOutput, Unit: pricing.UnitPerMillion, Amount: 25},
			},
		},
	}
}

// The unpriced ≠ zero-cost contract through the event builder: a request
// with no pricing resolved (or no priceable tokens) must come out with
// CostNanos nil — never a stamped 0.
func TestBuildEvent_CostStamping(t *testing.T) {
	pricer := testPricer(map[string]*pricing.Pricing{"pr-1": testSheet()})
	body := []byte(`{}`)
	tr := stubTranslator{tokens: sdkusage.Tokens{"input": 1000, "output": 200}}

	lc := lifecycle.NewContext("req-1", "pipeline", time.Now())
	lc.PricingID, lc.PricingName = "pr-1", "anthropic-opus"
	lc.ProviderName = "anthropic"
	lc.Translator = tr

	ev := buildEvent(lc, 200, "", "", body, pricer)
	if ev.Provider != "anthropic" || ev.Pricing != "anthropic-opus" {
		t.Fatalf("slugs: %q/%q", ev.Provider, ev.Pricing)
	}
	if ev.CostNanos == nil {
		t.Fatal("priced request: CostNanos nil")
	}
	// 1000×5000 + 200×25000 nanos
	if *ev.CostNanos != 10_000_000 {
		t.Fatalf("cost = %d", *ev.CostNanos)
	}
	if ev.CostBreakdown["tokens.input"] != 5_000_000 || ev.CostBreakdown["tokens.output"] != 5_000_000 {
		t.Fatalf("breakdown: %+v", ev.CostBreakdown)
	}

	// No pricing resolved → cost fields absent entirely.
	lc2 := lifecycle.NewContext("req-2", "pipeline", time.Now())
	lc2.Translator = tr
	ev2 := buildEvent(lc2, 200, "", "", body, pricer)
	if ev2.CostNanos != nil || ev2.CostBreakdown != nil || ev2.Pricing != "" {
		t.Fatalf("unpriced leaked cost fields: %+v", ev2)
	}

	// Pricing stamped but no tokens (error / LogOnly row) → slug kept for
	// audit, cost stays nil.
	lc3 := lifecycle.NewContext("req-3", "pipeline", time.Now())
	lc3.PricingID, lc3.PricingName = "pr-1", "anthropic-opus"
	ev3 := buildEvent(lc3, 0, "no_keys", "no healthy keys", nil, pricer)
	if ev3.CostNanos != nil {
		t.Fatalf("token-less row priced: %+v", ev3.CostNanos)
	}
	if ev3.Pricing != "anthropic-opus" {
		t.Fatalf("pricing slug dropped: %q", ev3.Pricing)
	}

	// Pricing id stamped but the sheet is gone from the snapshot → unpriced.
	lc4 := lifecycle.NewContext("req-4", "pipeline", time.Now())
	lc4.PricingID, lc4.PricingName = "gone", "deleted-sheet"
	lc4.Translator = tr
	ev4 := buildEvent(lc4, 200, "", "", body, pricer)
	if ev4.CostNanos != nil {
		t.Fatal("deleted sheet: want unpriced")
	}

	// Nil pricer (partial wiring) is safe and prices nothing.
	ev5 := buildEvent(lc, 200, "", "", body, nil)
	if ev5.CostNanos != nil {
		t.Fatal("nil pricer: want unpriced")
	}
}

// A genuine $0 (priced meters, zero counts) stamps CostNanos = 0 — present,
// not absent.
func TestBuildEvent_ZeroCostIsPriced(t *testing.T) {
	pricer := testPricer(map[string]*pricing.Pricing{"pr-1": testSheet()})
	body := []byte(`{}`)

	lc := lifecycle.NewContext("req-z", "pipeline", time.Now())
	lc.PricingID, lc.PricingName = "pr-1", "anthropic-opus"
	lc.Translator = stubTranslator{tokens: sdkusage.Tokens{"input": 0, "output": 0}}

	ev := buildEvent(lc, 200, "", "", body, pricer)
	if ev.CostNanos == nil || *ev.CostNanos != 0 {
		t.Fatalf("zero-cost row: %+v", ev.CostNanos)
	}
}
