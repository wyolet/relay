// Package ratelimit is the domain layer for the RateLimit entity — a named
// rule set (requests / tokens / concurrency caps) attachable to a Policy,
// ProviderKey, or Model.
//
// The Attachment type is also defined here because the entities that attach
// to a RateLimit depend on this package; reverse imports would create cycles.
package ratelimit

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/meta"
)

// RateLimit is a named rule set. All three OwnerKinds are valid:
//   - system: bundled, operator-immutable (e.g. inference-api, control-api).
//   - provider: auto-injected upstream-tier mirror; Owner.ID = Provider id.
//   - user: operator-defined.
type RateLimit struct {
	Meta meta.Metadata `json:"metadata" yaml:"metadata"`
	Spec Spec          `json:"spec"     yaml:"spec"`
}

// Spec carries the rule list and an enable flag.
type Spec struct {
	Rules   []Rule `json:"rules"             yaml:"rules"             validate:"required,min=1,dive"`
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"` // nil = true
}

// Rule is one cap. A RateLimit with N rules produces N concurrent buckets at
// request time. Strategy is per-rule — there is no spec-level default fallback.
//
// Window is a time.Duration; on the wire it accepts a human-readable string
// ("30s", "1m") or an int64 nanosecond count for round-tripping with the
// storage format.
type Rule struct {
	Meter    Meter         `json:"meter"    yaml:"meter"    validate:"required,oneof=requests concurrency tokens tokens.input tokens.output tokens.cache_read tokens.cache_creation tokens.reasoning tokens.server_tool_use_input tokens.server_tool_use_output"`
	Amount   int64         `json:"amount"   yaml:"amount"   validate:"required,gt=0"`
	Window   time.Duration `json:"window"   yaml:"window"   validate:"required,gt=0"`
	Strategy Strategy      `json:"strategy" yaml:"strategy" validate:"required,oneof=token-bucket sliding-window fixed-window leaky-bucket session-window"`
}

// Meter is the dimension a Rule counts.
type Meter string

// Closed set of recognised meters. Bare "tokens" sums every token sub-meter;
// "tokens.<key>" targets one. "concurrency" ignores Strategy.
const (
	MeterRequests               Meter = "requests"
	MeterConcurrency            Meter = "concurrency"
	MeterTokens                 Meter = "tokens"
	MeterTokensInput            Meter = "tokens.input"
	MeterTokensOutput           Meter = "tokens.output"
	MeterTokensCacheRead        Meter = "tokens.cache_read"
	MeterTokensCacheCreation    Meter = "tokens.cache_creation"
	MeterTokensReasoning        Meter = "tokens.reasoning"
	MeterTokensServerToolUseIn  Meter = "tokens.server_tool_use_input"
	MeterTokensServerToolUseOut Meter = "tokens.server_tool_use_output"
)

// Strategy is the algorithm used to enforce a Rule.
type Strategy string

const (
	StrategyTokenBucket   Strategy = "token-bucket"
	StrategySlidingWindow Strategy = "sliding-window"
	StrategyFixedWindow   Strategy = "fixed-window"
	StrategyLeakyBucket   Strategy = "leaky-bucket"
	// StrategySessionWindow anchors on first request, runs for `window`,
	// then idles until the next request anchors a fresh window. Used for
	// session-quota patterns like Anthropic's 5-hour limit.
	StrategySessionWindow Strategy = "session-window"
)

// IsEnabled returns true when Enabled is unset or explicitly true.
func (r *RateLimit) IsEnabled() bool { return r.Spec.Enabled == nil || *r.Spec.Enabled }

// Validate runs intra-row rules via the shared meta.Validator and enforces
// the RateLimit-specific owner shape:
//   - Owner.Kind is required (any of system/provider/user).
//   - Owner.Kind=provider requires Owner.ID (the Provider id).
//
// Cross-entity checks (provider-owned RLs reference an existing Provider;
// system mirrors are unique per tier) live in the composition layer.
func (r *RateLimit) Validate() error {
	if err := meta.Validator.Struct(r); err != nil {
		return err
	}
	switch r.Meta.Owner.Kind {
	case meta.OwnerSystem, meta.OwnerUser:
	case meta.OwnerProvider:
		if r.Meta.Owner.ID == "" {
			return fmt.Errorf("ratelimit %q: owner.id is required (provider id)", r.Meta.Name)
		}
	case meta.OwnerHost:
		if r.Meta.Owner.ID == "" {
			return fmt.Errorf("ratelimit %q: owner.id is required (host id)", r.Meta.Name)
		}
	default:
		return fmt.Errorf("ratelimit %q: owner.kind required (system|provider|host|user)", r.Meta.Name)
	}
	return nil
}
