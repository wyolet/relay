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

// sourceRE matches the optional attribution source field.
var sourceRE = regexp.MustCompile(`^attribution\.[a-z][a-z0-9_]*$`)

const maxRateLimitWindow = 30 * 24 * time.Hour

func validate(s *snapshot) error {
	// An entirely empty catalog is valid — the relay starts without config and
	// is populated via the admin API. Only validate cross-entity consistency
	// when there is at least one object present.
	if len(s.providers) == 0 && len(s.secrets) == 0 && len(s.pools) == 0 &&
		len(s.models) == 0 && len(s.routes) == 0 && len(s.rateLimits) == 0 {
		return nil
	}
	if len(s.providers) == 0 {
		return errors.New("at least one Provider required")
	}

	if err := validateSecrets(s); err != nil {
		return err
	}
	if err := validatePools(s); err != nil {
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
	return nil
}

func validateSecrets(s *snapshot) error {
	for _, sec := range s.secrets {
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
			p, ok := s.providers[sec.Spec.Provider]
			if ok && p.Spec.Kind != PKOllama {
				return fmt.Errorf("Secret %q: exactly one of valueFrom.env or value required", sec.Metadata.Name)
			}
		}
		if sec.Spec.Provider == "" {
			return fmt.Errorf("Secret %q: provider required", sec.Metadata.Name)
		}
		if _, ok := s.providers[sec.Spec.Provider]; !ok {
			return fmt.Errorf("Secret %q: unknown provider %q", sec.Metadata.Name, sec.Spec.Provider)
		}
	}
	return nil
}

func validatePools(s *snapshot) error {
	for _, pool := range s.pools {
		if pool.Spec.Provider == "" {
			return fmt.Errorf("Pool %q: provider required", pool.Metadata.Name)
		}
		prov, ok := s.providers[pool.Spec.Provider]
		if !ok {
			return fmt.Errorf("Pool %q: unknown provider %q", pool.Metadata.Name, pool.Spec.Provider)
		}
		for _, secName := range pool.Spec.Secrets {
			sec, ok := s.secrets[secName]
			if !ok {
				return fmt.Errorf("Pool %q: unknown secret %q", pool.Metadata.Name, secName)
			}
			if sec.Spec.Provider != pool.Spec.Provider {
				return fmt.Errorf("Pool %q: secret %q belongs to provider %q, not %q", pool.Metadata.Name, secName, sec.Spec.Provider, pool.Spec.Provider)
			}
		}
		effective := s.secretsForPool(pool)
		authRequired := prov.Spec.Kind == PKOpenAI || prov.Spec.Kind == PKAnthropic
		if authRequired && len(effective) == 0 {
			return fmt.Errorf("Pool %q: provider %q requires auth but pool has no effective secrets", pool.Metadata.Name, pool.Spec.Provider)
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
		if p.Spec.Default {
			defaults++
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
		if p.Spec.DefaultPool == "" {
			continue
		}
		pool, ok := s.pools[p.Spec.DefaultPool]
		if !ok {
			return fmt.Errorf("Provider %q: defaultPool %q does not exist", p.Metadata.Name, p.Spec.DefaultPool)
		}
		if pool.Spec.Provider != p.Metadata.Name {
			return fmt.Errorf("Provider %q: defaultPool %q belongs to provider %q", p.Metadata.Name, p.Spec.DefaultPool, pool.Spec.Provider)
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
		if _, ok := s.providers[m.Spec.Provider]; !ok {
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
		if r.Spec.Default {
			defaults++
		}
		if len(r.Spec.Models) == 0 {
			return fmt.Errorf("Route %q: at least one model required", r.Metadata.Name)
		}
		for _, mn := range r.Spec.Models {
			if _, ok := s.models[mn]; !ok {
				return fmt.Errorf("Route %q: unknown model %q", r.Metadata.Name, mn)
			}
		}
	}
	if defaults > 1 {
		return errors.New("at most one Route may be default")
	}
	return nil
}

func validateRateLimits(s *snapshot) error {
	for _, rl := range s.rateLimits {
		if rl.Spec.Strategy != StrategySlidingWindow {
			return fmt.Errorf("RateLimit %q: unsupported strategy %q (must be sliding-window)", rl.Metadata.Name, rl.Spec.Strategy)
		}
		if rl.Spec.Window <= 0 {
			return fmt.Errorf("RateLimit %q: window must be > 0", rl.Metadata.Name)
		}
		if rl.Spec.Window > maxRateLimitWindow {
			return fmt.Errorf("RateLimit %q: window %v exceeds maximum of 30 days", rl.Metadata.Name, rl.Spec.Window)
		}
		// Reject explicitly empty rules list (nil/omitted is fine — it falls back to legacy).
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
			if r.Source != "" && !sourceRE.MatchString(r.Source) {
				return fmt.Errorf("RateLimit %q rule[%d] (meter=%s): source %q must match attribution.<key>", rl.Metadata.Name, i, r.Meter, r.Source)
			}
			meterSeen[r.Meter]++
		}
		for m, cnt := range meterSeen {
			if cnt > 1 {
				slog.Warn("RateLimit has duplicate meter in rules", "name", rl.Metadata.Name, "meter", m, "count", cnt)
			}
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
			if _, ok := s.rateLimits[a.Ref]; !ok {
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
	for _, pool := range s.pools {
		if err := checkAttachments(KindPool, pool.Metadata.Name, pool.Spec.RateLimits); err != nil {
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

func validatePoolDefaultLimits(s *snapshot) error {
	for _, pool := range s.pools {
		prov, ok := s.providers[pool.Spec.Provider]
		if !ok {
			continue
		}
		authRequired := prov.Spec.Kind == PKOpenAI || prov.Spec.Kind == PKAnthropic
		if !authRequired || pool.Spec.SkipDefaultLimits {
			continue
		}
		hasRequests := false
		hasTokens := false

		// Check effective rules via snapshot expansion.
		checkRules := func(attachments []RateLimitAttachment) {
			for _, a := range attachments {
				rl, ok := s.rateLimits[a.Ref]
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
		checkRules(pool.Spec.RateLimits)
		for _, sec := range s.secretsForPool(pool) {
			checkRules(sec.Spec.RateLimits)
		}
		if !hasRequests || !hasTokens {
			return fmt.Errorf("Pool %q: auth-required provider needs at least one requests and one tokens rate-limit attachment (set skipDefaultLimits: true to opt out)", pool.Metadata.Name)
		}
	}
	return nil
}
