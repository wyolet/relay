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
