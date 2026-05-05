package configstore

import (
	"errors"
	"fmt"
	"net/url"
)

func validate(s *YAMLStore) error {
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

func validateSecrets(s *YAMLStore) error {
	for _, sec := range s.secrets {
		hasEnv := sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != ""
		hasVal := sec.Spec.Value != ""
		if hasEnv && hasVal {
			return fmt.Errorf("Secret %q: exactly one of valueFrom.env or value must be set, not both", sec.Metadata.Name)
		}
		if !hasEnv && !hasVal {
			// allow empty literal only for anonymous (ollama) providers
			if sec.Spec.ValueFrom != nil {
				// valueFrom set but env empty
				return fmt.Errorf("Secret %q: valueFrom.env must not be empty", sec.Metadata.Name)
			}
			// neither set — check provider kind
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

func validatePools(s *YAMLStore) error {
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
		// compute effective set
		effective := s.SecretsForPool(pool)
		authRequired := prov.Spec.Kind == PKOpenAI || prov.Spec.Kind == PKAnthropic
		if authRequired && len(effective) == 0 {
			return fmt.Errorf("Pool %q: provider %q requires auth but pool has no effective secrets", pool.Metadata.Name, pool.Spec.Provider)
		}
	}
	return nil
}

func validateProviders(s *YAMLStore) error {
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
		u, err := url.Parse(p.Spec.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("Provider %q: baseURL invalid (%q)", p.Metadata.Name, p.Spec.BaseURL)
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

func validateModels(s *YAMLStore) error {
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
	}
	return nil
}

func validateRoutes(s *YAMLStore) error {
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

func validateRateLimits(s *YAMLStore) error {
	for _, rl := range s.rateLimits {
		t := rl.Spec.Target
		switch t.Kind {
		case KindProvider:
			if _, ok := s.providers[t.Name]; !ok {
				return fmt.Errorf("RateLimit %q: unknown Provider target %q", rl.Metadata.Name, t.Name)
			}
		case KindModel:
			if _, ok := s.models[t.Name]; !ok {
				return fmt.Errorf("RateLimit %q: unknown Model target %q", rl.Metadata.Name, t.Name)
			}
		case KindRoute:
			if _, ok := s.routes[t.Name]; !ok {
				return fmt.Errorf("RateLimit %q: unknown Route target %q", rl.Metadata.Name, t.Name)
			}
		default:
			return fmt.Errorf("RateLimit %q: unsupported target kind %q", rl.Metadata.Name, t.Kind)
		}
		if rl.Spec.RPM == 0 && rl.Spec.TPM == 0 {
			return fmt.Errorf("RateLimit %q: at least one of rpm/tpm required", rl.Metadata.Name)
		}
	}
	return nil
}
