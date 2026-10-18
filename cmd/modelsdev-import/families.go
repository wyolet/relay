package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// familyList is the catalog's families.yaml allowlist of Model spec.family values.
type familyList struct {
	Families []struct {
		Name    string   `yaml:"name"`
		Aliases []string `yaml:"aliases"`
	} `yaml:"families"`
}

// UnmappedFamily is a model whose models.dev family matched no allowlisted family.
type UnmappedFamily struct {
	Provider, Model, Value string
}

func loadFamilies(path string) (*familyList, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("families allowlist: %w (point -families at the catalog's families.yaml)", err)
	}
	var f familyList
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("families allowlist %s: %w", path, err)
	}
	if len(f.Families) == 0 {
		return nil, fmt.Errorf("families allowlist %s: no families", path)
	}
	return &f, nil
}

// resolve maps a models.dev family onto an allowlisted family: exact name,
// exact alias, then the longest name and the longest alias the value extends
// as a lineage prefix. Returns "" when nothing matches.
func (f *familyList) resolve(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if f == nil || v == "" {
		return ""
	}
	for _, fam := range f.Families {
		if fam.Name == v {
			return fam.Name
		}
	}
	for _, fam := range f.Families {
		for _, a := range fam.Aliases {
			if a == v {
				return fam.Name
			}
		}
	}
	best, bestLen := "", 0
	for _, fam := range f.Families {
		if len(fam.Name) > bestLen && extendsLineage(v, fam.Name) {
			best, bestLen = fam.Name, len(fam.Name)
		}
	}
	if best != "" {
		return best
	}
	for _, fam := range f.Families {
		for _, a := range fam.Aliases {
			if len(a) > bestLen && extendsLineage(v, a) {
				best, bestLen = fam.Name, len(a)
			}
		}
	}
	return best
}

// extendsLineage reports whether v is prefix followed by "-", a digit or "."
// — so "qwen3.6" extends "qwen" but "olmo" does not extend "o".
func extendsLineage(v, prefix string) bool {
	if len(v) <= len(prefix) || !strings.HasPrefix(v, prefix) {
		return false
	}
	c := v[len(prefix)]
	return c == '-' || c == '.' || (c >= '0' && c <= '9')
}
