package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Load walks dir recursively, parses every *.yaml / *.yml file
// (multi-document supported), and returns the validated Config.
func Load(dir string) (*Config, error) {
	cfg := newConfig()

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		return loadFile(path, cfg)
	})
	if err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

type rawDoc struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

func loadFile(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	docIdx := 0
	for {
		var raw rawDoc
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%s [doc %d]: %w", path, docIdx, err)
		}
		// Skip empty docs (trailing --- in a file).
		if raw.Kind == "" && raw.APIVersion == "" {
			docIdx++
			continue
		}
		if raw.APIVersion != APIVersion {
			return fmt.Errorf("%s [doc %d]: unsupported apiVersion %q (want %q)", path, docIdx, raw.APIVersion, APIVersion)
		}
		if raw.Metadata.Name == "" {
			return fmt.Errorf("%s [doc %d]: metadata.name required", path, docIdx)
		}
		if err := dispatchKind(path, docIdx, &raw, cfg); err != nil {
			return err
		}
		docIdx++
	}
	return nil
}

func dispatchKind(path string, idx int, raw *rawDoc, cfg *Config) error {
	name := raw.Metadata.Name
	switch raw.Kind {
	case KindProvider:
		var spec ProviderSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Provider %s: %w", path, idx, name, err)
		}
		if _, dup := cfg.Providers[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Provider %q", path, idx, name)
		}
		cfg.Providers[name] = &Provider{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindModel:
		var spec ModelSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Model %s: %w", path, idx, name, err)
		}
		if _, dup := cfg.Models[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Model %q", path, idx, name)
		}
		cfg.Models[name] = &Model{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindRoute:
		var spec RouteSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Route %s: %w", path, idx, name, err)
		}
		if _, dup := cfg.Routes[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Route %q", path, idx, name)
		}
		cfg.Routes[name] = &Route{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindRateLimit:
		var spec RateLimitSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] RateLimit %s: %w", path, idx, name, err)
		}
		if _, dup := cfg.RateLimits[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate RateLimit %q", path, idx, name)
		}
		cfg.RateLimits[name] = &RateLimit{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	default:
		return fmt.Errorf("%s [doc %d]: unknown kind %q", path, idx, raw.Kind)
	}
	return nil
}
