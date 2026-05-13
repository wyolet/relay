package manifest

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Document is a discriminated union over all supported wire kinds. Exactly one
// of the fields below is non-nil after successful parsing.
type Document struct {
	Provider *ProviderDTO
	Host     *HostDTO
	Model    *ModelDTO
	HostKey  *HostKeyDTO
	Policy   *PolicyDTO
	RateLimit *RateLimitDTO
	RelayKey *RelayKeyDTO
	Pricing  *PricingDTO
}

// Kind returns the kind string of the contained document.
func (d Document) Kind() string {
	switch {
	case d.Provider != nil:
		return "Provider"
	case d.Host != nil:
		return "Host"
	case d.Model != nil:
		return "Model"
	case d.HostKey != nil:
		return "HostKey"
	case d.Policy != nil:
		return "Policy"
	case d.RateLimit != nil:
		return "RateLimit"
	case d.RelayKey != nil:
		return "RelayKey"
	case d.Pricing != nil:
		return "Pricing"
	default:
		return ""
	}
}

// rawEnvelope is decoded first to determine the kind, then the spec node is
// decoded into the kind-specific struct.
type rawEnvelope struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Metadata   WireMeta  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

// Parse reads one or more YAML documents from r and returns a Document slice.
// Multi-doc YAML (--- separated) is fully supported.
func Parse(r io.Reader) ([]Document, error) {
	dec := yaml.NewDecoder(r)
	var docs []Document
	docIdx := 0
	for {
		var env rawEnvelope
		err := dec.Decode(&env)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("wire: doc %d: %w", docIdx, err)
		}
		if env.Kind == "" && env.APIVersion == "" {
			docIdx++
			continue
		}
		if env.APIVersion != APIVersion {
			return nil, fmt.Errorf("wire: doc %d: unsupported apiVersion %q (want %q)", docIdx, env.APIVersion, APIVersion)
		}
		// Skip kinds owned by sibling subsystems that share the config tree.
		// Identity (User, Group, Role) lives in config/users/ alongside catalog
		// YAML; the catalog seeder must walk past it without erroring.
		if isForeignKind(env.Kind) {
			docIdx++
			continue
		}
		if env.Metadata.Name == "" {
			return nil, fmt.Errorf("wire: doc %d kind=%s: metadata.name required", docIdx, env.Kind)
		}
		doc, err := dispatchKind(&env)
		if err != nil {
			return nil, fmt.Errorf("wire: doc %d kind=%s name=%s: %w", docIdx, env.Kind, env.Metadata.Name, err)
		}
		docs = append(docs, doc)
		docIdx++
	}
	return docs, nil
}

// LoadDir walks dir for .yaml / .yml files and parses each. Documents from
// all files are merged into a single slice. File order is deterministic
// (lexicographic).
func LoadDir(dir string) ([]Document, error) {
	var all []Document
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
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("wire: open %s: %w", path, err)
		}
		defer f.Close()
		docs, err := Parse(f)
		if err != nil {
			return fmt.Errorf("wire: %s: %w", path, err)
		}
		all = append(all, docs...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return all, nil
}

func dispatchKind(env *rawEnvelope) (Document, error) {
	switch env.Kind {
	case "Provider":
		var spec ProviderSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Provider: &ProviderDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Host":
		var spec HostSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Host: &HostDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Model":
		var spec ModelSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Model: &ModelDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "HostKey":
		var spec HostKeySpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{HostKey: &HostKeyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Policy":
		var spec PolicySpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Policy: &PolicyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "RateLimit":
		var spec RateLimitSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{RateLimit: &RateLimitDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "RelayKey":
		var spec RelayKeySpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{RelayKey: &RelayKeyDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	case "Pricing":
		var spec PricingSpec
		if err := env.Spec.Decode(&spec); err != nil {
			return Document{}, err
		}
		return Document{Pricing: &PricingDTO{APIVersion: env.APIVersion, Kind: env.Kind, Metadata: env.Metadata, Spec: spec}}, nil

	default:
		return Document{}, fmt.Errorf("unknown kind %q", env.Kind)
	}
}

// isForeignKind reports kinds owned by sibling subsystems that the catalog
// manifest loader should pass through silently. Keep this list narrow —
// silent skips can mask typos in catalog YAML.
func isForeignKind(kind string) bool {
	switch kind {
	case "User", "Group", "Role":
		return true
	}
	return false
}
