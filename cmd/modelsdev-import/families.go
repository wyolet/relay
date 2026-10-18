package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// familyList is the catalog's families.yaml allowlist of Model spec.family values, each with the spec.productLine values valid under it.
type familyList struct {
	Families []familyEntry `yaml:"families"`
}

type familyEntry struct {
	Name    string        `yaml:"name"`
	Aliases []string      `yaml:"aliases"`
	Lines   []productLine `yaml:"lines"`
}

// productLine is one allowed spec.productLine. Aliases only map models.dev values onto the line; they are never valid spec values themselves.
type productLine struct {
	Name    string   `yaml:"name"`
	Aliases []string `yaml:"aliases"`
}

// UnmappedFamily is a model whose models.dev family matched no allowlisted family.
type UnmappedFamily struct {
	Provider, Model, Value string
}

// UnmappedLine is a model whose models.dev family names a product line the resolved family does not list.
type UnmappedLine struct {
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
// as a lineage prefix. Returns "" when nothing matches. lineCandidate is the
// part of the value that may name a product line: the matched alias, or the
// remainder after the family name; empty on an exact name match.
func (f *familyList) resolve(v string) (family, lineCandidate string) {
	v = strings.ToLower(strings.TrimSpace(v))
	if f == nil || v == "" {
		return "", ""
	}
	for _, fam := range f.Families {
		if fam.Name == v {
			return fam.Name, ""
		}
	}
	for _, fam := range f.Families {
		for _, a := range fam.Aliases {
			if a == v {
				return fam.Name, a
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
		rest := v[bestLen:]
		if rest[0] == '-' || rest[0] == '.' {
			rest = rest[1:]
		}
		return best, rest
	}
	for _, fam := range f.Families {
		for _, a := range fam.Aliases {
			if len(a) > bestLen && extendsLineage(v, a) {
				best, bestLen, lineCandidate = fam.Name, len(a), a
			}
		}
	}
	return best, lineCandidate
}

// line maps a line candidate onto one of family's lines, by name and then by line alias, dropping trailing "-segment"s until one matches ("codex-mini" → "codex"). Returns "" when nothing matches.
func (f *familyList) line(family, candidate string) string {
	if f == nil || candidate == "" {
		return ""
	}
	for _, fam := range f.Families {
		if fam.Name != family {
			continue
		}
		for c := candidate; ; {
			if l := fam.lineFor(c); l != "" {
				return l
			}
			i := strings.LastIndexByte(c, '-')
			if i < 0 {
				return ""
			}
			c = c[:i]
		}
	}
	return ""
}

func (fam familyEntry) lineFor(c string) string {
	for _, l := range fam.Lines {
		if l.Name == c {
			return l.Name
		}
	}
	for _, l := range fam.Lines {
		for _, a := range l.Aliases {
			if a == c {
				return l.Name
			}
		}
	}
	return ""
}

// isVersion reports whether a line candidate is only digits and dots, as in "qwen3.6" → "3.6": a version, not an unknown line.
func isVersion(s string) bool {
	return s != "" && strings.Trim(s, "0123456789.") == ""
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
