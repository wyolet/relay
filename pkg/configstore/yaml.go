package configstore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

type YAMLStore struct {
	providers  map[string]*Provider
	models     map[string]*Model
	routes     map[string]*Route
	rateLimits map[string]*RateLimit
}

func LoadYAML(dir string) (*YAMLStore, error) {
	s := &YAMLStore{
		providers:  map[string]*Provider{},
		models:     map[string]*Model{},
		routes:     map[string]*Route{},
		rateLimits: map[string]*RateLimit{},
	}

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
		return loadFile(path, s)
	})
	if err != nil {
		return nil, err
	}

	if err := validate(s); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *YAMLStore) ProviderByName(name string) (*Provider, bool) {
	p, ok := s.providers[name]
	return p, ok
}

func (s *YAMLStore) ModelByName(name string) (*Model, bool) {
	m, ok := s.models[name]
	return m, ok
}

func (s *YAMLStore) RouteByName(name string) (*Route, bool) {
	r, ok := s.routes[name]
	return r, ok
}

func (s *YAMLStore) RateLimitByName(name string) (*RateLimit, bool) {
	rl, ok := s.rateLimits[name]
	return rl, ok
}

func (s *YAMLStore) Providers() []*Provider {
	out := make([]*Provider, 0, len(s.providers))
	for _, p := range s.providers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *YAMLStore) Models() []*Model {
	out := make([]*Model, 0, len(s.models))
	for _, m := range s.models {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *YAMLStore) Routes() []*Route {
	out := make([]*Route, 0, len(s.routes))
	for _, r := range s.routes {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *YAMLStore) RateLimits() []*RateLimit {
	out := make([]*RateLimit, 0, len(s.rateLimits))
	for _, rl := range s.rateLimits {
		out = append(out, rl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out
}

func (s *YAMLStore) DefaultProvider() *Provider {
	for _, p := range s.providers {
		if p.Spec.Default {
			return p
		}
	}
	return nil
}

func (s *YAMLStore) DefaultRoute() *Route {
	for _, r := range s.routes {
		if r.Spec.Default {
			return r
		}
	}
	return nil
}

func (s *YAMLStore) ProviderForModel(modelName string) (*Provider, bool) {
	m, ok := s.models[modelName]
	if !ok {
		return nil, false
	}
	p, ok := s.providers[m.Spec.Provider]
	return p, ok
}

type rawDoc struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

func loadFile(path string, s *YAMLStore) error {
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
		if err := dispatchKind(path, docIdx, &raw, s); err != nil {
			return err
		}
		docIdx++
	}
	return nil
}

func dispatchKind(path string, idx int, raw *rawDoc, s *YAMLStore) error {
	name := raw.Metadata.Name
	switch raw.Kind {
	case KindProvider:
		var spec ProviderSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Provider %s: %w", path, idx, name, err)
		}
		if _, dup := s.providers[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Provider %q", path, idx, name)
		}
		s.providers[name] = &Provider{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindModel:
		var spec ModelSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Model %s: %w", path, idx, name, err)
		}
		if _, dup := s.models[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Model %q", path, idx, name)
		}
		s.models[name] = &Model{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindRoute:
		var spec RouteSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Route %s: %w", path, idx, name, err)
		}
		if _, dup := s.routes[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Route %q", path, idx, name)
		}
		s.routes[name] = &Route{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindRateLimit:
		var spec RateLimitSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] RateLimit %s: %w", path, idx, name, err)
		}
		if _, dup := s.rateLimits[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate RateLimit %q", path, idx, name)
		}
		s.rateLimits[name] = &RateLimit{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	default:
		return fmt.Errorf("%s [doc %d]: unknown kind %q", path, idx, raw.Kind)
	}
	return nil
}
