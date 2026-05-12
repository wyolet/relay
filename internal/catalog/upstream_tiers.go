package catalog

import "time"

// upstreamTier describes the hard rate limits published by an upstream provider
// for a named billing tier (e.g. "openai-tier-1", "anthropic-tier-3").
// Rules carry their own Window and Strategy; the snapshot loader copies them
// verbatim into the auto-injected RateLimit object.
type upstreamTier struct {
	Provider ProviderKind
	Name     string // e.g. "openai-tier-1"
	Rules    []RateLimitRule
}

// upstreamTiers is the static table of known upstream tier limits.
// Sources:
//   - OpenAI:     https://platform.openai.com/docs/guides/rate-limits
//   - Anthropic:  https://docs.anthropic.com/en/api/rate-limits
var upstreamTiers = []upstreamTier{
	{Provider: PKOpenAI, Name: "openai-tier-1", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 500, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 30000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKOpenAI, Name: "openai-tier-2", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 5000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 450000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKOpenAI, Name: "openai-tier-3", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 5000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 800000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKOpenAI, Name: "openai-tier-4", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 10000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 2000000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKOpenAI, Name: "openai-tier-5", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 10000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 30000000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKAnthropic, Name: "anthropic-tier-1", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 50, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 50000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKAnthropic, Name: "anthropic-tier-2", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 1000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 100000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKAnthropic, Name: "anthropic-tier-3", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 2000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 400000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
	{Provider: PKAnthropic, Name: "anthropic-tier-4", Rules: []RateLimitRule{
		{Meter: "requests", Amount: 4000, Window: time.Minute, Strategy: StrategySlidingWindow},
		{Meter: "tokens", Amount: 400000, Window: time.Minute, Strategy: StrategySlidingWindow},
	}},
}

// upstreamTiersByName is a fast lookup map built at init time.
var upstreamTiersByName map[string]*upstreamTier

// AllUpstreamTiers is the slice of tier name strings used in OpenAPI enum tags.
var AllUpstreamTiers []string

func init() {
	upstreamTiersByName = make(map[string]*upstreamTier, len(upstreamTiers))
	AllUpstreamTiers = make([]string, 0, len(upstreamTiers))
	for i := range upstreamTiers {
		upstreamTiersByName[upstreamTiers[i].Name] = &upstreamTiers[i]
		AllUpstreamTiers = append(AllUpstreamTiers, upstreamTiers[i].Name)
	}
}

// lookupUpstreamTier returns the tier with the given name, or nil if unknown.
func lookupUpstreamTier(name string) *upstreamTier {
	return upstreamTiersByName[name]
}

// copyRules returns a deep copy of the tier's rule slice so mutations on the
// injected RateLimit do not affect the static table.
func (t *upstreamTier) copyRules() []RateLimitRule {
	rules := make([]RateLimitRule, len(t.Rules))
	copy(rules, t.Rules)
	return rules
}
