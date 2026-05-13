package catalog

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// meterRE matches valid multi-rule meter strings.
// Accepted forms: requests, concurrency, tokens, tokens.<suffix>
var meterRE = regexp.MustCompile(`^(requests|concurrency|tokens|tokens\.[a-z][a-z0-9_]*)$`)

// pricingRateKeyRE matches valid Pricing.Rates keys.
var pricingRateKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_.]*$`)

// isoCurrencyRE matches 3-letter uppercase ISO 4217 currency codes.
var isoCurrencyRE = regexp.MustCompile(`^[A-Z]{3}$`)


const maxRateLimitWindow = 30 * 24 * time.Hour

func validate(s *snapshot) error {
	// An entirely empty catalog is valid — the relay starts without config and
	// is populated via the admin API. Only validate cross-entity consistency
	// when there is at least one object present.
	if len(s.providers) == 0 && len(s.secrets) == 0 && len(s.policies) == 0 &&
		len(s.models) == 0 && len(s.routes) == 0 && len(s.rateLimits) == 0 &&
		len(s.relayKeys) == 0 {
		return nil
	}
	if len(s.providers) == 0 {
		return errors.New("at least one Provider required")
	}

	if err := validateSecrets(s); err != nil {
		return err
	}
	if err := validatePolicies(s); err != nil {
		return err
	}
	if err := validateProviders(s); err != nil {
		return err
	}
	if err := validateModels(s); err != nil {
		return err
	}
	if err := validateRoutes(s); err != nil {
		return err
	}
	if err := validateRateLimits(s); err != nil {
		return err
	}
	if err := validateRelayKeys(s); err != nil {
		return err
	}
	if err := validatePassthrough(s); err != nil {
		return err
	}
	return nil
}

func validatePassthrough(s *snapshot) error {
	pt := s.passthrough
	if pt == nil {
		return nil // unset == default; nothing to check
	}
	if pt.Metadata.Name != PassthroughSingletonName {
		return fmt.Errorf("Passthrough %q: name must be %q (singleton)", pt.Metadata.Name, PassthroughSingletonName)
	}
	if pt.Spec.Unauthenticated.Enabled {
		if !pt.Spec.Enabled {
			return fmt.Errorf("Passthrough: unauthenticated.enabled requires spec.enabled=true")
		}
		if pt.Spec.Unauthenticated.BucketBy == "" {
			return fmt.Errorf("Passthrough: unauthenticated.bucketBy required when enabled")
		}
		switch pt.Spec.Unauthenticated.BucketBy {
		case PassthroughBucketByCredentialHash:
		default:
			return fmt.Errorf("Passthrough: unsupported unauthenticated.bucketBy %q", pt.Spec.Unauthenticated.BucketBy)
		}
	}
	switch pt.Spec.Models.Mode {
	case PassthroughModelsModeAll:
		// allow list ignored
	case PassthroughModelsModeAllowlist:
		if len(pt.Spec.Models.Allow) == 0 {
			return fmt.Errorf("Passthrough: models.allow must be non-empty when mode=allowlist")
		}
		for _, name := range pt.Spec.Models.Allow {
			if _, ok := s.models[name]; !ok {
				return fmt.Errorf("Passthrough: models.allow references unknown model %q", name)
			}
		}
	case "":
		return fmt.Errorf("Passthrough: models.mode required (one of all, allowlist)")
	default:
		return fmt.Errorf("Passthrough: unsupported models.mode %q", pt.Spec.Models.Mode)
	}
	if pt.Spec.Enabled && len(pt.Spec.Transports) == 0 {
		return fmt.Errorf("Passthrough: transports must be non-empty when spec.enabled=true")
	}
	for _, t := range pt.Spec.Transports {
		switch t {
		case "http", "ws", "amqp", "pubsub":
		default:
			return fmt.Errorf("Passthrough: unsupported transport %q", t)
		}
	}
	return nil
}

var relayKeyHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validateRelayKeys(s *snapshot) error {
	seenHash := make(map[string]string, len(s.relayKeys))
	for _, k := range s.relayKeys {
		if err := validateOwner("RelayKey", k.Metadata.Name, k.Metadata.Owner); err != nil {
			return err
		}
		if k.Spec.KeyHash == "" {
			return fmt.Errorf("RelayKey %q: keyHash required", k.Metadata.Name)
		}
		if !relayKeyHashRE.MatchString(k.Spec.KeyHash) {
			return fmt.Errorf("RelayKey %q: keyHash must be 64 lowercase hex chars (sha256)", k.Metadata.Name)
		}
		if other, dup := seenHash[k.Spec.KeyHash]; dup {
			return fmt.Errorf("RelayKey %q: duplicate keyHash with %q", k.Metadata.Name, other)
		}
		seenHash[k.Spec.KeyHash] = k.Metadata.Name
		if k.Spec.PolicyRef != "" {
			if _, ok := s.policyByID(k.Spec.PolicyRef); !ok {
				return fmt.Errorf("RelayKey %q: unknown policyRef %q", k.Metadata.Name, k.Spec.PolicyRef)
			}
		}
	}
	return nil
}

func validateSecrets(s *snapshot) error {
	for _, sec := range s.secrets {
		if err := validateOwner("Secret", sec.Metadata.Name, sec.Metadata.Owner); err != nil {
			return err
		}
		if sec.Spec.Tier != "" && lookupUpstreamTier(sec.Spec.Tier) == nil {
			return fmt.Errorf("Secret %q: tier %q is not a known upstream tier", sec.Metadata.Name, sec.Spec.Tier)
		}
		hasEnv := sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != ""
		hasVal := sec.Spec.Value != ""
		// Stored-mode secrets (written via UpsertSecretStored) carry their
		// resolved plaintext in sec.Resolved but have neither ValueFrom.Env
		// nor Spec.Value populated in the JSON spec column.  Accept them as
		// long as they resolved successfully.
		hasResolved := sec.Resolved != ""
		if hasEnv && hasVal {
			return fmt.Errorf("Secret %q: exactly one of valueFrom.env or value must be set, not both", sec.Metadata.Name)
		}
		if !hasEnv && !hasVal && !hasResolved {
			if sec.Spec.ValueFrom != nil {
				return fmt.Errorf("Secret %q: valueFrom.env must not be empty", sec.Metadata.Name)
			}
			p, ok := s.providerByID(sec.Spec.Provider)
			if ok && p.Spec.Kind != PKOllama {
				return fmt.Errorf("Secret %q: exactly one of valueFrom.env or value required", sec.Metadata.Name)
			}
		}
		if sec.Spec.Provider == "" {
			return fmt.Errorf("Secret %q: provider required", sec.Metadata.Name)
		}
		if _, ok := s.providerByID(sec.Spec.Provider); !ok {
			return fmt.Errorf("Secret %q: unknown provider %q", sec.Metadata.Name, sec.Spec.Provider)
		}
	}
	return nil
}

func validatePolicies(s *snapshot) error {
	for _, policy := range s.policies {
		if err := validateOwner("Policy", policy.Metadata.Name, policy.Metadata.Owner); err != nil {
			return err
		}
		if policy.Spec.Provider == "" {
			return fmt.Errorf("Policy %q: provider required", policy.Metadata.Name)
		}
		prov, ok := s.providerByID(policy.Spec.Provider)
		if !ok {
			return fmt.Errorf("Policy %q: unknown provider %q", policy.Metadata.Name, policy.Spec.Provider)
		}
		for _, secRef := range policy.Spec.Secrets {
			sec, ok := s.secretByID(secRef)
			if !ok {
				return fmt.Errorf("Policy %q: unknown secret %q", policy.Metadata.Name, secRef)
			}
			if sec.Spec.Provider != policy.Spec.Provider {
				return fmt.Errorf("Policy %q: secret %q belongs to provider %q, not %q", policy.Metadata.Name, sec.Metadata.Name, sec.Spec.Provider, policy.Spec.Provider)
			}
		}
		for _, modelRef := range policy.Spec.Models {
			m, ok := s.modelByID(modelRef)
			if !ok {
				return fmt.Errorf("Policy %q: unknown model %q", policy.Metadata.Name, modelRef)
			}
			if m.Spec.Provider != policy.Spec.Provider {
				return fmt.Errorf("Policy %q: model %q belongs to provider %q, not %q", policy.Metadata.Name, m.Metadata.Name, m.Spec.Provider, policy.Spec.Provider)
			}
		}
		effective := s.secretsForPolicy(policy)
		authRequired := prov.Spec.Kind == PKOpenAI || prov.Spec.Kind == PKAnthropic
		if authRequired && len(effective) == 0 {
			return fmt.Errorf("Policy %q: provider %q requires auth but policy has no effective secrets (passthrough is now configured globally via /control/passthrough, not per-policy)", policy.Metadata.Name, policy.Spec.Provider)
		}
	}
	return nil
}

func validateURL(field, val string) error {
	u, err := url.Parse(val)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("%s invalid (%q)", field, val)
	}
	return nil
}

func validatePricing(context string, pr *Pricing) error {
	if pr == nil {
		return nil
	}
	if pr.Currency != "" && !isoCurrencyRE.MatchString(pr.Currency) {
		return fmt.Errorf("%s: pricing.currency %q must be a 3-letter uppercase ISO code or empty", context, pr.Currency)
	}
	switch pr.Unit {
	case PricingUnitPerMillion, PricingUnitPerThousand, PricingUnitPerUnit, "":
	default:
		return fmt.Errorf("%s: pricing.unit %q must be one of per_million, per_thousand, per_unit", context, pr.Unit)
	}
	for k, v := range pr.Rates {
		if !pricingRateKeyRE.MatchString(k) {
			return fmt.Errorf("%s: pricing.rates key %q must match ^[a-z][a-z0-9_.]*$", context, k)
		}
		if v < 0 {
			return fmt.Errorf("%s: pricing.rates[%q] must be >= 0", context, k)
		}
	}
	return nil
}

func validateProviders(s *snapshot) error {
	defaults := 0
	for _, p := range s.providers {
		if err := validateOwner("Provider", p.Metadata.Name, p.Metadata.Owner); err != nil {
			return err
		}
		if p.Spec.Default {
			defaults++
		}
		if p.Spec.DefaultTier != "" && lookupUpstreamTier(p.Spec.DefaultTier) == nil {
			return fmt.Errorf("Provider %q: defaultTier %q is not a known upstream tier", p.Metadata.Name, p.Spec.DefaultTier)
		}
		switch p.Spec.Kind {
		case PKOllama, PKOpenAI, PKAnthropic:
		default:
			return fmt.Errorf("Provider %q: unsupported kind %q", p.Metadata.Name, p.Spec.Kind)
		}
		if p.Spec.BaseURL == "" {
			return fmt.Errorf("Provider %q: baseURL required", p.Metadata.Name)
		}
		if err := validateURL(fmt.Sprintf("Provider %q: baseURL", p.Metadata.Name), p.Spec.BaseURL); err != nil {
			return err
		}
		// Validate optional URL fields.
		for field, val := range map[string]string{
			"homepageURL":   p.Spec.HomepageURL,
			"docsURL":       p.Spec.DocsURL,
			"consoleURL":    p.Spec.ConsoleURL,
			"statusPageURL": p.Spec.StatusPageURL,
			"logoURL":       p.Spec.LogoURL,
		} {
			if val != "" {
				if err := validateURL(fmt.Sprintf("Provider %q: %s", p.Metadata.Name, field), val); err != nil {
					return err
				}
			}
		}
		if err := validatePricing(fmt.Sprintf("Provider %q", p.Metadata.Name), p.Spec.DefaultPricing); err != nil {
			return err
		}
	}
	if defaults > 1 {
		return errors.New("at most one Provider may be default")
	}
	for _, p := range s.providers {
		if p.Spec.DefaultPolicy == "" {
			continue
		}
		policy, ok := s.policyByID(p.Spec.DefaultPolicy)
		if !ok {
			return fmt.Errorf("Provider %q: defaultPolicy %q does not exist", p.Metadata.Name, p.Spec.DefaultPolicy)
		}
		if policy.Spec.Provider != p.Metadata.ID {
			return fmt.Errorf("Provider %q: defaultPolicy %q belongs to provider %q", p.Metadata.Name, p.Spec.DefaultPolicy, policy.Spec.Provider)
		}
	}
	return nil
}

func validateModels(s *snapshot) error {
	// Build a lookup of all names and aliases to check for collisions.
	allNames := make(map[string]string) // name/alias -> model name
	for _, m := range s.models {
		allNames[m.Metadata.Name] = m.Metadata.Name
	}
	for _, m := range s.models {
		if err := validateOwner("Model", m.Metadata.Name, m.Metadata.Owner); err != nil {
			return err
		}
		for _, alias := range m.Spec.Aliases {
			if existing, ok := allNames[alias]; ok && existing != m.Metadata.Name {
				return fmt.Errorf("Model %q: alias %q collides with model or alias %q", m.Metadata.Name, alias, existing)
			}
			allNames[alias] = m.Metadata.Name
		}
	}

	for _, m := range s.models {
		if m.Spec.Provider == "" {
			return fmt.Errorf("Model %q: provider required", m.Metadata.Name)
		}
		if _, ok := s.providerByID(m.Spec.Provider); !ok {
			return fmt.Errorf("Model %q: unknown provider %q", m.Metadata.Name, m.Spec.Provider)
		}
		if m.Spec.UpstreamName == "" {
			return fmt.Errorf("Model %q: upstreamName required", m.Metadata.Name)
		}
		// Validate context window fields.
		if m.Spec.ContextWindow < 0 || m.Spec.ContextWindowInput < 0 || m.Spec.ContextWindowOutput < 0 || m.Spec.ContextWindowTotal < 0 {
			return fmt.Errorf("Model %q: context window values must be non-negative", m.Metadata.Name)
		}
		// Validate pricing.
		if err := validatePricing(fmt.Sprintf("Model %q", m.Metadata.Name), m.Spec.Pricing); err != nil {
			return err
		}
		// Validate deprecation status.
		if m.Spec.Deprecation != nil {
			switch m.Spec.Deprecation.Status {
			case "", "active", "deprecated", "sunset":
			default:
				return fmt.Errorf("Model %q: deprecation.status %q must be one of active, deprecated, sunset", m.Metadata.Name, m.Spec.Deprecation.Status)
			}
		}
		// Validate optional URL fields.
		if m.Spec.ProviderModelPageURL != "" {
			if err := validateURL(fmt.Sprintf("Model %q: providerModelPageURL", m.Metadata.Name), m.Spec.ProviderModelPageURL); err != nil {
				return err
			}
		}
		// Validate alias uniqueness within the model.
		seen := make(map[string]struct{}, len(m.Spec.Aliases))
		for _, alias := range m.Spec.Aliases {
			aliasLower := strings.ToLower(alias)
			if _, dup := seen[aliasLower]; dup {
				return fmt.Errorf("Model %q: duplicate alias %q", m.Metadata.Name, alias)
			}
			seen[aliasLower] = struct{}{}
		}
	}
	return nil
}

func validateRoutes(s *snapshot) error {
	defaults := 0
	for _, r := range s.routes {
		if err := validateOwner("Route", r.Metadata.Name, r.Metadata.Owner); err != nil {
			return err
		}
		if r.Spec.Default {
			defaults++
		}
		if len(r.Spec.Models) == 0 {
			return fmt.Errorf("Route %q: at least one model required", r.Metadata.Name)
		}
		for _, mn := range r.Spec.Models {
			if _, ok := s.modelByID(mn); !ok {
				return fmt.Errorf("Route %q: unknown model %q", r.Metadata.Name, mn)
			}
		}
	}
	if defaults > 1 {
		return errors.New("at most one Route may be default")
	}
	return nil
}

var validStrategies = map[RateLimitStrategy]bool{
	StrategyTokenBucket:   true,
	StrategySlidingWindow: true,
	StrategyFixedWindow:   true,
	StrategyLeakyBucket:   true,
	StrategySessionWindow: true,
}

// validateOwner enforces the owner.kind / owner.id pairing for any resource.
// Empty kind is accepted (treated as default per resource); set kinds must be
// known. id is required for provider-owned resources and forbidden for
// system-owned resources.
func validateOwner(kindLabel, name string, o Owner) error {
	switch o.Kind {
	case OwnerSystem, OwnerProvider, OwnerUser, "":
	default:
		return fmt.Errorf("%s %q: metadata.owner.kind %q is not valid (must be system, provider, or user)", kindLabel, name, o.Kind)
	}
	if o.Kind == OwnerProvider && o.ID == "" {
		return fmt.Errorf("%s %q: metadata.owner.id required when kind=provider", kindLabel, name)
	}
	if o.Kind == OwnerSystem && o.ID != "" {
		return fmt.Errorf("%s %q: metadata.owner.id must be empty when kind=system", kindLabel, name)
	}
	return nil
}

func validateRateLimits(s *snapshot) error {
	for _, rl := range s.rateLimits {
		if err := validateOwner("RateLimit", rl.Metadata.Name, rl.Metadata.Owner); err != nil {
			return err
		}

		// System-owned objects may have empty rules — they are config ceilings
		// that operators populate after deployment. Skip rule validation for them.
		if rl.Metadata.Owner.Kind == OwnerSystem && len(rl.Spec.Rules) == 0 {
			continue
		}
		// Reject explicitly empty rules list.
		if rl.Spec.Rules != nil && len(rl.Spec.Rules) == 0 {
			return fmt.Errorf("RateLimit %q: rules must be non-empty", rl.Metadata.Name)
		}
		rules := rl.Spec.NormalizedRules()
		if len(rules) == 0 {
			return fmt.Errorf("RateLimit %q: rules must be non-empty", rl.Metadata.Name)
		}
		meterSeen := make(map[string]int)
		for i, r := range rules {
			if !meterRE.MatchString(r.Meter) {
				return fmt.Errorf("RateLimit %q rule[%d]: meter %q invalid (must match requests|concurrency|tokens|tokens.<suffix>)", rl.Metadata.Name, i, r.Meter)
			}
			if r.Amount <= 0 {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): amount must be > 0", rl.Metadata.Name, i, r.Meter)
			}
			// Every rule must declare its own window (no spec-level fallback).
			if r.Window <= 0 {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): window is required on each rule (no spec-level default)", rl.Metadata.Name, i, r.Meter)
			}
			if r.Window > maxRateLimitWindow {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): window %v exceeds maximum of 30 days", rl.Metadata.Name, i, r.Meter, r.Window)
			}
			// Every rule must declare its own strategy.
			if r.Strategy == "" {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): strategy is required on each rule", rl.Metadata.Name, i, r.Meter)
			}
			if !validStrategies[r.Strategy] {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): unsupported strategy %q", rl.Metadata.Name, i, r.Meter, r.Strategy)
			}
			// tokens / tokens.<suffix> meters are post-hoc (amount known only after
			// the upstream responds). Only sliding-window has a Commit-time increment
			// path; token-bucket / leaky-bucket / fixed-window would silently no-op.
			if (r.Meter == string(MeterTokens) || strings.HasPrefix(r.Meter, "tokens.")) &&
				r.Strategy != StrategySlidingWindow {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): strategy %q is not supported for tokens meter; only sliding-window is supported (tokens are counted post-hoc)",
					rl.Metadata.Name, i, r.Meter, r.Strategy)
			}
			meterSeen[r.Meter+"@"+r.Window.String()]++
		}
		for k, cnt := range meterSeen {
			if cnt > 1 {
				slog.Warn("RateLimit has duplicate (meter,window) in rules", "name", rl.Metadata.Name, "meter_window", k, "count", cnt)
			}
		}
		if err := validateAgainstCeiling(rl, s); err != nil {
			return err
		}
	}

	if err := validateAttachments(s); err != nil {
		return err
	}
	if err := validatePoolDefaultLimits(s); err != nil {
		return err
	}
	return nil
}

func validateAttachments(s *snapshot) error {
	checkAttachments := func(kind Kind, name string, attachments []RateLimitAttachment) error {
		for _, a := range attachments {
			if _, ok := s.rateLimitByID(a.Ref); !ok {
				return fmt.Errorf("%s %q: rateLimits ref %q does not exist", kind, name, a.Ref)
			}
		}
		return nil
	}
	for _, sec := range s.secrets {
		if err := checkAttachments(KindSecret, sec.Metadata.Name, sec.Spec.RateLimits); err != nil {
			return err
		}
	}
	for _, policy := range s.policies {
		if err := checkAttachments(KindPolicy, policy.Metadata.Name, policy.Spec.RateLimits); err != nil {
			return err
		}
	}
	for _, m := range s.models {
		if err := checkAttachments(KindModel, m.Metadata.Name, m.Spec.RateLimits); err != nil {
			return err
		}
	}
	return nil
}

// validateAgainstCeiling checks that a non-system RateLimit does not exceed the
// config ceiling defined by the "inference" system-mirrored RateLimit. Ceiling
// matching is lenient: only rules with the same (meter, effective-window) pair
// are compared; unmatched meters are allowed. If the ceiling object is absent or
// disabled, validation is skipped entirely.
//
// We check against "inference" only (not "inference-proxy") for v1 simplicity.
// Operators using passthrough-allowed keys can override via env if they need a
// more permissive ceiling in a future release.
func validateAgainstCeiling(rl *RateLimit, s *snapshot) error {
	// System/provider-owned objects (the ceilings themselves) are exempt.
	if rl.Metadata.Owner.Kind == OwnerSystem || rl.Metadata.Owner.Kind == OwnerProvider {
		return nil
	}

	ceiling, ok := s.rateLimits["inference"]
	if !ok || !IsEnabled(ceiling.Spec.Enabled) {
		return nil
	}

	// Build a lookup map: (meter, window) -> amount for the ceiling.
	type ceilingKey struct {
		meter  string
		window time.Duration
	}
	ceilingMap := make(map[ceilingKey]int64)
	for _, cr := range ceiling.Spec.NormalizedRules() {
		ceilingMap[ceilingKey{cr.Meter, cr.Window}] = cr.Amount
	}

	for i, r := range rl.Spec.NormalizedRules() {
		if max, found := ceilingMap[ceilingKey{r.Meter, r.Window}]; found {
			if r.Amount > max {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s window=%s amount=%d): exceeds ceiling \"inference\" (max=%d)",
					rl.Metadata.Name, i, r.Meter, r.Window, r.Amount, max)
			}
		}
	}
	return nil
}

func validatePoolDefaultLimits(s *snapshot) error {
	for _, policy := range s.policies {
		prov, ok := s.providerByID(policy.Spec.Provider)
		if !ok {
			continue
		}
		authRequired := prov.Spec.Kind == PKOpenAI || prov.Spec.Kind == PKAnthropic
		if !authRequired || policy.Spec.SkipDefaultLimits {
			continue
		}
		hasRequests := false
		hasTokens := false

		// Check effective rules via snapshot expansion.
		checkRules := func(attachments []RateLimitAttachment) {
			for _, a := range attachments {
				rl, ok := s.rateLimitByID(a.Ref)
				if !ok {
					continue
				}
				for _, r := range rl.Spec.NormalizedRules() {
					if r.Meter == string(MeterRequests) {
						hasRequests = true
					}
					if r.Meter == string(MeterTokens) || (len(r.Meter) > len("tokens.") && r.Meter[:len("tokens.")] == "tokens.") {
						hasTokens = true
					}
				}
			}
		}
		checkRules(policy.Spec.RateLimits)
		for _, sec := range s.secretsForPolicy(policy) {
			checkRules(sec.Spec.RateLimits)
		}
		if !hasRequests || !hasTokens {
			return fmt.Errorf("Policy %q: auth-required provider needs at least one requests and one tokens rate-limit attachment (set skipDefaultLimits: true to opt out)", policy.Metadata.Name)
		}
	}
	return nil
}
