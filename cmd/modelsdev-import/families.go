package main

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/wyolet/relay/app/families"
)

// familyList is the catalog's families.yaml allowlist, with the importer's mapping of models.dev family values onto it.
type familyList families.File

// UnmappedFamily is a model whose models.dev family matched no allowlisted family.
type UnmappedFamily struct {
	Provider, Model, Value string
}

// UnmappedLine is a model whose models.dev family names a product line the resolved family does not list.
type UnmappedLine struct {
	Provider, Model, Value string
}

func loadFamilies(path string) (*familyList, error) {
	f, err := families.Load(path)
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return nil, fmt.Errorf("families allowlist: %w (point -families at the catalog's families.yaml)", err)
	}
	if err != nil {
		return nil, fmt.Errorf("families allowlist %w", err)
	}
	if len(f.Families) == 0 {
		return nil, fmt.Errorf("families allowlist %s: no families", path)
	}
	return (*familyList)(f), nil
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
		return best, trimLeadingVersion(v[bestLen:])
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
			if l := lineFor(fam, c); l != "" {
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

func lineFor(fam families.Family, c string) string {
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

// trimLeadingVersion drops the separator and any version glued to the family name, so "3-coder" (from "qwen3-coder") yields "coder" and "3.6" yields "".
func trimLeadingVersion(rest string) string {
	rest = strings.TrimLeft(rest, "-.")
	rest = strings.TrimLeft(rest, "0123456789.")
	return strings.TrimLeft(rest, "-")
}

// lineMiss reports whether a line candidate that mapped to no line is worth a report: the family declares lines, and the candidate is not one of the family's own aliases (chatgpt, a family-only alias, names no line).
func (f *familyList) lineMiss(family, candidate, line string) bool {
	if f == nil || line != "" || candidate == "" {
		return false
	}
	for _, fam := range f.Families {
		if fam.Name != family {
			continue
		}
		if len(fam.Lines) == 0 {
			return false
		}
		for _, a := range fam.Aliases {
			if a == candidate {
				return false
			}
		}
		return true
	}
	return false
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
