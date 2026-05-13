package catalog

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

type YAMLStore struct {
	snap *snapshot
}

func LoadYAML(dir string) (*YAMLStore, error) {
	snap, err := loadYAMLRaw(dir)
	if err != nil {
		return nil, err
	}

	if err := resolveSecrets(snap); err != nil {
		return nil, err
	}

	ensureSnapshotIDs(snap)
	snap.buildByIDIndexes()
	if _, err := resolveRefs(snap); err != nil {
		return nil, err
	}

	if err := validate(snap); err != nil {
		return nil, err
	}
	snap.injectUpstreamTierRateLimits()
	snap.buildEffectivePricing()
	snap.buildByIDIndexes()
	return &YAMLStore{snap: snap}, nil
}

// LoadYAMLForSeed reads the YAML tree in name-form: no id stamping, no
// cross-ref resolution, no validation. Seed consumes this and resolves refs
// itself against a name→id index it builds from the current PG state.
func LoadYAMLForSeed(dir string) (*YAMLStore, error) {
	snap, err := loadYAMLRaw(dir)
	if err != nil {
		return nil, err
	}
	if err := resolveSecrets(snap); err != nil {
		return nil, err
	}
	return &YAMLStore{snap: snap}, nil
}

// loadYAMLRaw parses every YAML in dir into a snapshot without stamping ids,
// resolving cross-refs, or validating. Cross-ref Spec fields stay in name
// form. The seed CLI uses this so it can build its own name→id index against
// the live PG state and resolve refs at upsert time.
func loadYAMLRaw(dir string) (*snapshot, error) {
	snap := newSnapshot()
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
		return loadFile(path, snap)
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func resolveSecrets(snap *snapshot) error {
	var literalNames []string
	for name, sec := range snap.secrets {
		switch {
		case sec.Spec.ValueFrom != nil && sec.Spec.ValueFrom.Env != "":
			val, ok := os.LookupEnv(sec.Spec.ValueFrom.Env)
			if !ok || val == "" {
				return fmt.Errorf("Secret %q: env var %q not set or empty", name, sec.Spec.ValueFrom.Env)
			}
			sec.Resolved = val
		case sec.Spec.Value != "":
			sec.Resolved = sec.Spec.Value
			sec.UsedLiteral = true
			literalNames = append(literalNames, name)
		case sec.Spec.Value == "" && sec.Spec.ValueFrom == nil:
			sec.Resolved = ""
			sec.UsedLiteral = true
			literalNames = append(literalNames, name)
		}
		if sec.Resolved != "" {
			sum := sha256.Sum256([]byte(sec.Resolved))
			sec.KeyHash = fmt.Sprintf("%x", sum[:6])
		}
	}
	if len(literalNames) > 0 {
		sort.Strings(literalNames)
		slog.Warn("secrets used literal value (deprecated, encrypted storage in M5)", "secrets", literalNames)
	}
	return nil
}

func (s *YAMLStore) ProviderByName(name string) (*Provider, bool)   { return s.snap.providerByName(name) }
func (s *YAMLStore) ModelByName(name string) (*Model, bool)          { return s.snap.modelByName(name) }
func (s *YAMLStore) RouteByName(name string) (*Route, bool)          { return s.snap.routeByName(name) }
func (s *YAMLStore) RateLimitByName(name string) (*RateLimit, bool)  { return s.snap.rateLimitByName(name) }
func (s *YAMLStore) SecretByName(name string) (*Secret, bool)        { return s.snap.secretByName(name) }
func (s *YAMLStore) PolicyByName(name string) (*Policy, bool)            { return s.snap.policyByName(name) }

func (s *YAMLStore) ProviderByID(id string) (*Provider, bool)   { return s.snap.providerByID(id) }
func (s *YAMLStore) ModelByID(id string) (*Model, bool)         { return s.snap.modelByID(id) }
func (s *YAMLStore) RouteByID(id string) (*Route, bool)         { return s.snap.routeByID(id) }
func (s *YAMLStore) RateLimitByID(id string) (*RateLimit, bool) { return s.snap.rateLimitByID(id) }
func (s *YAMLStore) SecretByID(id string) (*Secret, bool)       { return s.snap.secretByID(id) }
func (s *YAMLStore) PolicyByID(id string) (*Policy, bool)       { return s.snap.policyByID(id) }
func (s *YAMLStore) RelayKeyByID(id string) (*RelayKey, bool)   { return s.snap.relayKeyByID(id) }
func (s *YAMLStore) Providers() []*Provider                          { return s.snap.listProviders() }
func (s *YAMLStore) Models() []*Model                                { return s.snap.listModels() }
func (s *YAMLStore) Routes() []*Route                                { return s.snap.listRoutes() }
func (s *YAMLStore) RateLimits() []*RateLimit                        { return s.snap.listRateLimits() }
func (s *YAMLStore) Secrets() []*Secret                              { return s.snap.listSecrets() }
func (s *YAMLStore) Policies() []*Policy                                  { return s.snap.listPolicies() }
func (s *YAMLStore) DefaultProvider() *Provider                      { return s.snap.defaultProvider() }
func (s *YAMLStore) DefaultRoute() *Route                            { return s.snap.defaultRoute() }
func (s *YAMLStore) ProviderForModel(modelName string) (*Provider, bool) {
	return s.snap.providerForModel(modelName)
}
func (s *YAMLStore) SecretsForPolicy(p *Policy) []*Secret { return s.snap.secretsForPolicy(p) }
func (s *YAMLStore) RateLimitsForRequest(provider *Provider, policy *Policy, model *Model, secret *Secret) []ResolvedRule {
	return s.snap.rateLimitsForRequest(provider, policy, model, secret)
}
func (s *YAMLStore) EffectivePricing(modelName string) (*Pricing, bool) {
	return s.snap.effectivePricingByModel(modelName)
}
func (s *YAMLStore) RelayKeyByName(name string) (*RelayKey, bool) { return s.snap.relayKeyByName(name) }
func (s *YAMLStore) RelayKeyByHash(hash string) (*RelayKey, bool) { return s.snap.relayKeyByHash(hash) }
func (s *YAMLStore) RelayKeys() []*RelayKey                       { return s.snap.listRelayKeys() }
func (s *YAMLStore) Passthrough() *Passthrough                    { return s.snap.passthroughOrDefault() }

type rawDoc struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

func loadFile(path string, snap *snapshot) error {
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
		if err := dispatchKind(path, docIdx, &raw, snap); err != nil {
			return err
		}
		docIdx++
	}
	return nil
}

func dispatchKind(path string, idx int, raw *rawDoc, snap *snapshot) error {
	name := raw.Metadata.Name
	switch raw.Kind {
	case KindProvider:
		var spec ProviderSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Provider %s: %w", path, idx, name, err)
		}
		if _, dup := snap.providers[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Provider %q", path, idx, name)
		}
		snap.providers[name] = &Provider{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindModel:
		var spec ModelSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Model %s: %w", path, idx, name, err)
		}
		if _, dup := snap.models[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Model %q", path, idx, name)
		}
		snap.models[name] = &Model{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindRoute:
		var spec RouteSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Route %s: %w", path, idx, name, err)
		}
		if _, dup := snap.routes[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Route %q", path, idx, name)
		}
		snap.routes[name] = &Route{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindRateLimit:
		var spec RateLimitSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] RateLimit %s: %w", path, idx, name, err)
		}
		if _, dup := snap.rateLimits[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate RateLimit %q", path, idx, name)
		}
		snap.rateLimits[name] = &RateLimit{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindSecret:
		var spec SecretSpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Secret %s: %w", path, idx, name, err)
		}
		if _, dup := snap.secrets[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Secret %q", path, idx, name)
		}
		snap.secrets[name] = &Secret{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	case KindPolicy:
		var spec PolicySpec
		if err := raw.Spec.Decode(&spec); err != nil {
			return fmt.Errorf("%s [doc %d] Policy %s: %w", path, idx, name, err)
		}
		if _, dup := snap.policies[name]; dup {
			return fmt.Errorf("%s [doc %d]: duplicate Policy %q", path, idx, name)
		}
		snap.policies[name] = &Policy{APIVersion: raw.APIVersion, Kind: raw.Kind, Metadata: raw.Metadata, Spec: spec}

	default:
		// Identity-owned kinds live alongside catalog YAML in the same seed
		// directory; identity.LoadYAML picks them up separately.
		if raw.Kind == "User" {
			return nil
		}
		return fmt.Errorf("%s [doc %d]: unknown kind %q", path, idx, raw.Kind)
	}
	return nil
}
