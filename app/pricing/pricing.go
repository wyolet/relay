// Package pricing is the domain layer for the Pricing entity — a named rate
// sheet owned by a Host and applied to one or more Models. One Pricing row
// holds every rate that bills against a (model, host) pair: input, output,
// cache_read, cache_creation, reasoning, audio_*, and any context-tier variants.
//
// Pricing is owner=host so that "Anthropic's price list" and "Bedrock's
// price list" are two distinct entities even when they cover overlapping
// Models. TargetModelIDs lets one rate sheet cover a whole tier (claude-4
// family etc.) without duplicating rows.
package pricing

import (
	"fmt"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/sdk/usage"
)

// Pricing is a rate sheet. Owner.Kind must be OwnerHost; Owner.ID is the
// Host id.
type Pricing struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the rate list, the target model set, currency, and an enable
// flag.
type Spec struct {
	Currency       string   `json:"currency"             yaml:"currency"             validate:"required"`
	TargetModelIDs []string `json:"targetModels"         yaml:"targetModels"         validate:"required,min=1,dive,required"`
	Rates          []Rate   `json:"rates"                yaml:"rates"                validate:"required,min=1,dive"`
	Enabled        *bool    `json:"enabled,omitempty"    yaml:"enabled,omitempty"`
}

// Rate is one priced meter. AboveTokens=0 is the base tier; AboveTokens>0 is
// the rate charged once the request's billable token count exceeds that
// threshold. The billing-time picker walks rates for the meter and applies
// the largest qualifying threshold.
type Rate struct {
	Meter       Meter   `json:"meter"                 yaml:"meter"                 validate:"required,oneof=tokens.input tokens.output tokens.cache_read tokens.cache_creation tokens.reasoning tokens.audio_input tokens.audio_output tokens.accepted_prediction tokens.rejected_prediction tokens.server_tool_use_input tokens.server_tool_use_output"`
	Unit        Unit    `json:"unit"                  yaml:"unit"                  validate:"required,oneof=per_million per_unit"`
	Amount      float64 `json:"amount"                yaml:"amount"                validate:"required,gt=0"`
	AboveTokens int     `json:"aboveTokens,omitempty" yaml:"aboveTokens,omitempty" validate:"gte=0"`
}

// Meter is the dimension a Rate prices. Closed set; new meters require a
// schema bump (intentional — billing surface area should grow deliberately).
type Meter string

const (
	MeterTokensInput               Meter = "tokens.input"
	MeterTokensOutput              Meter = "tokens.output"
	MeterTokensCacheRead           Meter = "tokens.cache_read"
	MeterTokensCacheCreation       Meter = "tokens.cache_creation"
	MeterTokensReasoning           Meter = "tokens.reasoning"
	MeterTokensAudioInput          Meter = "tokens.audio_input"
	MeterTokensAudioOutput         Meter = "tokens.audio_output"
	MeterTokensAcceptedPrediction  Meter = "tokens.accepted_prediction"
	MeterTokensRejectedPrediction  Meter = "tokens.rejected_prediction"
	MeterTokensServerToolUseInput  Meter = "tokens.server_tool_use_input"
	MeterTokensServerToolUseOutput Meter = "tokens.server_tool_use_output"
)

// MeterForUsageKey maps a usage.Tokens key (as emitted by
// Adapter.ExtractTokens) to its corresponding pricing Meter. Returns
// the meter and true if known, or "" and false for unpriced keys.
func MeterForUsageKey(k string) (Meter, bool) {
	switch k {
	case "input":
		return MeterTokensInput, true
	case "output":
		return MeterTokensOutput, true
	case "cache_read":
		return MeterTokensCacheRead, true
	case "cache_creation":
		return MeterTokensCacheCreation, true
	case "reasoning":
		return MeterTokensReasoning, true
	case "audio_input":
		return MeterTokensAudioInput, true
	case "audio_output":
		return MeterTokensAudioOutput, true
	case "accepted_prediction":
		return MeterTokensAcceptedPrediction, true
	case "rejected_prediction":
		return MeterTokensRejectedPrediction, true
	case "server_tool_use_input":
		return MeterTokensServerToolUseInput, true
	case "server_tool_use_output":
		return MeterTokensServerToolUseOutput, true
	}
	return "", false
}

// Unit is how Amount is interpreted. per_million is the common case for
// token rates (Amount=2.50 means $2.50 per 1M tokens). per_unit is for
// future meters like per-image or per-call.
type Unit string

const (
	UnitPerMillion Unit = "per_million"
	UnitPerUnit    Unit = "per_unit"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (p *Pricing) IsEnabled() bool { return p.Spec.Enabled == nil || *p.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the Pricing-specific invariants:
//   - Owner.Kind == OwnerHost and Owner.ID is set (the Host id).
//   - No two Rates share the same (Meter, AboveTokens) — would be ambiguous
//     at billing time.
//
// Cross-entity checks (TargetModelIDs resolve, no two enabled Pricings
// claim the same (model, host) pair) live in the catalog composition layer.
func (p *Pricing) Validate() error {
	if err := meta.Validator.Struct(p); err != nil {
		return err
	}
	if p.Meta.Owner.Kind != meta.OwnerHost {
		return fmt.Errorf("pricing %q: owner.kind must be host", p.Meta.Name)
	}
	if p.Meta.Owner.ID == "" {
		return fmt.Errorf("pricing %q: owner.id is required (host id)", p.Meta.Name)
	}
	seen := make(map[string]struct{}, len(p.Spec.Rates))
	for i, r := range p.Spec.Rates {
		key := fmt.Sprintf("%s|%d", r.Meter, r.AboveTokens)
		if _, dup := seen[key]; dup {
			return fmt.Errorf("pricing %q: rates[%d]: duplicate (meter=%s, aboveTokens=%d)", p.Meta.Name, i, r.Meter, r.AboveTokens)
		}
		seen[key] = struct{}{}
	}
	return nil
}

// RateFor returns the rate that should bill against the given meter and
// request-side token count. Walks Rates for the meter and picks the row
// with the largest AboveTokens that is still ≤ tokens. Returns (nil, false)
// if no rate exists for the meter.
func (p *Pricing) RateFor(meter Meter, tokens int) (*Rate, bool) {
	var best *Rate
	for i := range p.Spec.Rates {
		r := &p.Spec.Rates[i]
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

// Cost computes the total cost (in Spec.Currency units, typically USD)
// for the given Tokens map. Tier selection uses the input token count —
// the conventional axis for context-length tiers across major providers.
// Keys not mapped by MeterForUsageKey are silently skipped (unpriced
// dimension). Returns 0 when Pricing is disabled or carries no rates.
func (p *Pricing) Cost(tokens usage.Tokens) float64 {
	if p == nil || !p.IsEnabled() || len(tokens) == 0 {
		return 0
	}
	tier := int(tokens["input"])
	var total float64
	for key, count := range tokens {
		meter, ok := MeterForUsageKey(key)
		if !ok {
			continue
		}
		rate, ok := p.RateFor(meter, tier)
		if !ok {
			continue
		}
		switch rate.Unit {
		case UnitPerMillion:
			total += float64(count) / 1_000_000 * rate.Amount
		case UnitPerUnit:
			total += float64(count) * rate.Amount
		}
	}
	return total
}
