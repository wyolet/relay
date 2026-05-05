package config

import (
	"errors"
	"fmt"
	"net/url"
)

func (c *Config) Validate() error {
	if len(c.Providers) == 0 {
		return errors.New("at least one Provider required")
	}

	if err := c.validateProviders(); err != nil {
		return err
	}
	if err := c.validateModels(); err != nil {
		return err
	}
	if err := c.validateRoutes(); err != nil {
		return err
	}
	if err := c.validateRateLimits(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateProviders() error {
	defaults := 0
	for _, p := range c.Providers {
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

func (c *Config) validateModels() error {
	for _, m := range c.Models {
		if m.Spec.Provider == "" {
			return fmt.Errorf("Model %q: provider required", m.Metadata.Name)
		}
		if _, ok := c.Providers[m.Spec.Provider]; !ok {
			return fmt.Errorf("Model %q: unknown provider %q", m.Metadata.Name, m.Spec.Provider)
		}
		if m.Spec.UpstreamName == "" {
			return fmt.Errorf("Model %q: upstreamName required", m.Metadata.Name)
		}
	}
	return nil
}

func (c *Config) validateRoutes() error {
	defaults := 0
	for _, r := range c.Routes {
		if r.Spec.Default {
			defaults++
		}
		if len(r.Spec.Models) == 0 {
			return fmt.Errorf("Route %q: at least one model required", r.Metadata.Name)
		}
		for _, mn := range r.Spec.Models {
			if _, ok := c.Models[mn]; !ok {
				return fmt.Errorf("Route %q: unknown model %q", r.Metadata.Name, mn)
			}
		}
	}
	if defaults > 1 {
		return errors.New("at most one Route may be default")
	}
	return nil
}

func (c *Config) validateRateLimits() error {
	for _, rl := range c.RateLimits {
		t := rl.Spec.Target
		switch t.Kind {
		case KindProvider:
			if _, ok := c.Providers[t.Name]; !ok {
				return fmt.Errorf("RateLimit %q: unknown Provider target %q", rl.Metadata.Name, t.Name)
			}
		case KindModel:
			if _, ok := c.Models[t.Name]; !ok {
				return fmt.Errorf("RateLimit %q: unknown Model target %q", rl.Metadata.Name, t.Name)
			}
		case KindRoute:
			if _, ok := c.Routes[t.Name]; !ok {
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
