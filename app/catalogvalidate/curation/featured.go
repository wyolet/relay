package curation

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

// Featured is featured.yaml: an index of the models labeled
// metadata.labels.featured="true", grouped by owner provider. The labels stay
// the source of truth (the seed and UI read them); the index is generated
// from them and checked for drift.
type Featured struct {
	Description string             `yaml:"description,omitempty"`
	Providers   []FeaturedProvider `yaml:"providers"`
}

type FeaturedProvider struct {
	Provider string   `yaml:"provider"`
	Models   []string `yaml:"models"`
}

const featuredDescription = "Featured models — generated index of Model metadata.labels.featured. " +
	"Regenerate: catalog-validate --write-featured ./data. Not a seeded catalog kind; validate fails on drift."

// GenerateFeatured builds the index from docs, sorted on both levels so the
// file is stable.
func GenerateFeatured(docs []manifest.Document) Featured {
	byProvider := map[string][]string{}
	for _, d := range docs {
		if d.Model == nil || d.Model.Metadata.Labels["featured"] != "true" {
			continue
		}
		p := d.Model.Metadata.Owner.Name
		if p == "" {
			p = d.Model.Metadata.Owner.ID
		}
		byProvider[p] = append(byProvider[p], d.Model.Metadata.Name)
	}

	m := Featured{Description: featuredDescription}
	for p, models := range byProvider {
		sort.Strings(models)
		m.Providers = append(m.Providers, FeaturedProvider{Provider: p, Models: models})
	}
	sort.Slice(m.Providers, func(i, j int) bool { return m.Providers[i].Provider < m.Providers[j].Provider })
	return m
}

// CheckFeatured reports drift between want (the checked-in index) and the
// featured labels in docs, in both directions.
func CheckFeatured(docs []manifest.Document, want Featured) []catalogvalidate.Issue {
	got := GenerateFeatured(docs)

	key := func(provider, model string) string { return provider + "/" + model }
	gotSet := map[string]bool{}
	for _, p := range got.Providers {
		for _, m := range p.Models {
			gotSet[key(p.Provider, m)] = true
		}
	}
	wantSet := map[string]bool{}
	for _, p := range want.Providers {
		for _, m := range p.Models {
			wantSet[key(p.Provider, m)] = true
		}
	}

	var out []catalogvalidate.Issue
	for _, p := range got.Providers {
		for _, m := range p.Models {
			if !wantSet[key(p.Provider, m)] {
				out = append(out, catalogvalidate.Issue{
					Severity: catalogvalidate.SeverityError,
					Kind:     catalogvalidate.KindInvariant,
					Source:   catalogvalidate.Ref{Kind: "Model", Name: m, Field: "metadata.labels.featured"},
					Message:  fmt.Sprintf("labeled featured but absent from featured.yaml under provider %q — regenerate with -write-featured", p.Provider),
				})
			}
		}
	}
	for _, p := range want.Providers {
		for _, m := range p.Models {
			if !gotSet[key(p.Provider, m)] {
				out = append(out, catalogvalidate.Issue{
					Severity: catalogvalidate.SeverityError,
					Kind:     catalogvalidate.KindInvariant,
					Source:   catalogvalidate.Ref{Kind: "Model", Name: m},
					Message:  fmt.Sprintf("featured.yaml lists it under provider %q but no such model carries the featured label — fix the label or regenerate with -write-featured", p.Provider),
				})
			}
		}
	}
	return out
}

// LoadFeatured reads path; ok is false when the file doesn't exist, which
// skips the drift check.
func LoadFeatured(path string) (m Featured, ok bool, err error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Featured{}, false, nil
	}
	if err != nil {
		return Featured{}, false, err
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return Featured{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return m, true, nil
}

func WriteFeatured(path string, m Featured) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := yaml.NewEncoder(f)
	enc.SetIndent(4)
	if err := enc.Encode(m); err != nil {
		return err
	}
	return enc.Close()
}
