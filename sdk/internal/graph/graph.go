// Package graph is the SDK's internal catalog-discovery graph: a denormalized,
// cross-linked view of the embedded catalog (models, the hosts that serve them,
// and the providers that author them) built once from sdk/catalog.
//
// It is internal on purpose. Consumers reach it only through the public
// sdk/model, sdk/host, and sdk/provider packages, which re-export these types by
// alias and expose a Resolve entry point each. Keeping the graph here lets the
// three public packages cross-link (Model→Host, Host→Model, Model→Provider)
// without importing each other — which would be a cycle.
//
// JSON note: a Model nests its Author and Hosts one level deep, but every
// back-reference (Provider.Models, Host.Models) is a slug string, never a
// pointer — so any node marshals to JSON without a cycle.
package graph

import (
	"sort"

	"github.com/wyolet/relay/sdk/catalog"
)

// Graph is the built, cross-linked catalog. Resolution reuses the catalog
// engine (ic) for ref matching, then maps the matched slug to its node.
type Graph struct {
	ic        *catalog.IndexedCatalog
	models    map[string]*Model    // by slug
	hosts     map[string]*Host     // by name
	providers map[string]*Provider // by name
}

// Build constructs the graph from a loaded catalog.
func Build(ic *catalog.IndexedCatalog) *Graph {
	g := &Graph{
		ic:        ic,
		models:    map[string]*Model{},
		hosts:     map[string]*Host{},
		providers: map[string]*Provider{},
	}
	c := ic.Catalog

	for _, pi := range c.Providers {
		g.providers[pi.Name] = &Provider{
			Name:          pi.Name,
			DisplayName:   pi.DisplayName,
			Description:   pi.Description,
			HomepageURL:   pi.HomepageURL,
			DocsURL:       pi.DocsURL,
			StatusPageURL: pi.StatusPageURL,
			Icon:          pi.Icon,
		}
	}

	for _, mi := range c.Models {
		g.models[mi.MetadataName] = &Model{
			Slug:            mi.MetadataName,
			DisplayName:     mi.DisplayName,
			Description:     mi.Description,
			Family:          mi.Family,
			Version:         mi.Version,
			Capabilities:    Capabilities(mi.Capabilities),
			Modalities:      Modalities(mi.Modalities),
			ContextWindow:   ContextWindow{Input: mi.ContextWindowInput, Output: mi.ContextWindowOutput, Total: mi.ContextWindowTotal},
			MaxOutputTokens: mi.MaxOutputTokens,
			KnowledgeCutoff: mi.KnowledgeCutoff,
			ReleaseDate:     mi.ReleaseDate,
			License:         mi.License,
			Tags:            mi.Tags,
			Author:          g.providers[mi.Provider],
		}
	}

	for hi := range c.Hosts {
		ch := &c.Hosts[hi]
		h := &Host{
			Name:          ch.Name,
			DisplayName:   ch.DisplayName,
			Description:   ch.Description,
			BaseURL:       ch.BaseURL,
			HomepageURL:   ch.HomepageURL,
			DocsURL:       ch.DocsURL,
			ConsoleURL:    ch.ConsoleURL,
			StatusPageURL: ch.StatusPageURL,
			Icon:          ch.Icon,
		}
		g.hosts[ch.Name] = h
		for _, b := range ch.Models {
			m := g.models[b.MetadataName]
			if m == nil {
				// Thin node: a served slug with no metadata sidecar (e.g. a
				// catalog.json generated before enrichment). Carry what the
				// binding has.
				m = &Model{Slug: b.MetadataName}
				g.models[b.MetadataName] = m
			}
			if m.Name == "" {
				m.Name = b.Name
			}
			m.Featured = m.Featured || b.Featured
			if m.Author == nil && len(b.Providers) > 0 {
				m.Author = g.providerOrStub(b.Providers[0])
			}
			m.Aliases = unionStrings(m.Aliases, b.Aliases)
			m.Hosts = append(m.Hosts, ModelHost{Host: h, Adapter: b.Adapter, Pricing: ratesFrom(b.Pricing)})
			h.Models = append(h.Models, b.MetadataName)
		}
	}

	// Back-links + deterministic ordering.
	for slug, m := range g.models {
		if m.Author != nil {
			m.Author.Models = append(m.Author.Models, slug)
		}
		sort.Slice(m.Hosts, func(i, j int) bool { return m.Hosts[i].Host.Name < m.Hosts[j].Host.Name })
	}
	for _, h := range g.hosts {
		h.Models = sortedUnique(h.Models)
	}
	for _, p := range g.providers {
		p.Models = sortedUnique(p.Models)
	}
	return g
}

func (g *Graph) providerOrStub(name string) *Provider {
	if p := g.providers[name]; p != nil {
		return p
	}
	p := &Provider{Name: name}
	g.providers[name] = p
	return p
}

func ratesFrom(rs []catalog.Rate) []Rate {
	if len(rs) == 0 {
		return nil
	}
	out := make([]Rate, len(rs))
	for i, r := range rs {
		out[i] = Rate(r)
	}
	return out
}

func unionStrings(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			a = append(a, s)
			seen[s] = true
		}
	}
	return a
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
