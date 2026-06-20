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
	"fmt"
	"sort"
	"sync"

	"github.com/wyolet/relay/sdk/catalog"
)

// Provider is a model author (the vendor). Models lists the slugs it authors.
type Provider struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName,omitempty"`
	Description   string   `json:"description,omitempty"`
	HomepageURL   string   `json:"homepageURL,omitempty"`
	DocsURL       string   `json:"docsURL,omitempty"`
	StatusPageURL string   `json:"statusPageURL,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	Models        []string `json:"models,omitempty"`
}

// Host is a serving endpoint. Models lists the slugs it serves.
type Host struct {
	Name          string   `json:"name"`
	DisplayName   string   `json:"displayName,omitempty"`
	Description   string   `json:"description,omitempty"`
	BaseURL       string   `json:"baseURL"`
	HomepageURL   string   `json:"homepageURL,omitempty"`
	DocsURL       string   `json:"docsURL,omitempty"`
	ConsoleURL    string   `json:"consoleURL,omitempty"`
	StatusPageURL string   `json:"statusPageURL,omitempty"`
	Icon          string   `json:"icon,omitempty"`
	Models        []string `json:"models,omitempty"`
}

// ModelHost is one place a model is served: the host node plus the per-host
// serving terms (wire adapter + pricing) that live on the (model, host) edge.
type ModelHost struct {
	Host    *Host  `json:"host"`
	Adapter string `json:"adapter,omitempty"`
	Pricing []Rate `json:"pricing,omitempty"`
}

// Rate is one priced meter (mirror of catalog.Rate; convertible field-for-field).
type Rate struct {
	Meter       string  `json:"meter"`
	Unit        string  `json:"unit"`
	Amount      float64 `json:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty"`
}

// Capabilities mirrors catalog.Capabilities field-for-field (convertible).
type Capabilities struct {
	Chat              bool `json:"chat,omitempty"`
	Embeddings        bool `json:"embeddings,omitempty"`
	Streaming         bool `json:"streaming,omitempty"`
	Tools             bool `json:"tools,omitempty"`
	ParallelTools     bool `json:"parallelTools,omitempty"`
	Vision            bool `json:"vision,omitempty"`
	Audio             bool `json:"audio,omitempty"`
	PromptCache       bool `json:"promptCache,omitempty"`
	Reasoning         bool `json:"reasoning,omitempty"`
	JSONMode          bool `json:"jsonMode,omitempty"`
	StructuredOutputs bool `json:"structuredOutputs,omitempty"`
	Batch             bool `json:"batch,omitempty"`
	ComputerUse       bool `json:"computerUse,omitempty"`
	WebSearch         bool `json:"webSearch,omitempty"`
	FileInput         bool `json:"fileInput,omitempty"`
	AudioInput        bool `json:"audioInput,omitempty"`
	AudioOutput       bool `json:"audioOutput,omitempty"`
	SystemMessages    bool `json:"systemMessages,omitempty"`
	AssistantPrefill  bool `json:"assistantPrefill,omitempty"`
}

// Modalities mirrors catalog.Modalities (convertible).
type Modalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// ContextWindow is the model's token-window split.
type ContextWindow struct {
	Input  int `json:"input,omitempty"`
	Output int `json:"output,omitempty"`
	Total  int `json:"total,omitempty"`
}

// Model is the discovery node: the model plus its author and the hosts serving
// it. Name is the real provider wire name; Slug is our catalog metadata name.
type Model struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
	Family      string `json:"family,omitempty"`
	Version     string `json:"version,omitempty"`

	Capabilities  Capabilities  `json:"capabilities,omitempty"`
	Modalities    Modalities    `json:"modalities,omitempty"`
	ContextWindow ContextWindow `json:"contextWindow,omitempty"`

	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	KnowledgeCutoff string   `json:"knowledgeCutoff,omitempty"`
	ReleaseDate     string   `json:"releaseDate,omitempty"`
	License         string   `json:"license,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Featured        bool     `json:"featured,omitempty"`
	Aliases         []string `json:"aliases,omitempty"`

	Author *Provider   `json:"author,omitempty"`
	Hosts  []ModelHost `json:"hosts,omitempty"`
}

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

// ResolveModel returns the model node for a ref, aggregating every host that
// serves it. Ref forms match the catalog engine (bare slug, provider/model,
// wire name, alias). Cross-host matches are not ambiguous — they are one model.
func (g *Graph) ResolveModel(ref string) (*Model, error) {
	slug, err := g.ic.ResolveModelSlug(ref)
	if err != nil {
		return nil, err
	}
	m := g.models[slug]
	if m == nil {
		return nil, fmt.Errorf("graph: model %q resolved to slug %q with no node", ref, slug)
	}
	return m, nil
}

// ResolveHost returns the host node by name.
func (g *Graph) ResolveHost(ref string) (*Host, error) {
	if h := g.hosts[ref]; h != nil {
		return h, nil
	}
	return nil, fmt.Errorf("graph: host %q not found", ref)
}

// ResolveProvider returns the provider (author) node by name.
func (g *Graph) ResolveProvider(ref string) (*Provider, error) {
	if p := g.providers[ref]; p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("graph: provider %q not found", ref)
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

var (
	loadOnce sync.Once
	loaded   *Graph
	loadErr  error
)

// Load builds (once) the graph from the embedded catalog and caches it.
func Load() (*Graph, error) {
	loadOnce.Do(func() {
		ic, err := catalog.Load()
		if err != nil {
			loadErr = err
			return
		}
		loaded = Build(ic)
	})
	return loaded, loadErr
}

// Package-level convenience wrappers used by the public model/host/provider
// packages.

func ResolveModel(ref string) (*Model, error) {
	g, err := Load()
	if err != nil {
		return nil, err
	}
	return g.ResolveModel(ref)
}

func ResolveHost(ref string) (*Host, error) {
	g, err := Load()
	if err != nil {
		return nil, err
	}
	return g.ResolveHost(ref)
}

func ResolveProvider(ref string) (*Provider, error) {
	g, err := Load()
	if err != nil {
		return nil, err
	}
	return g.ResolveProvider(ref)
}
