package graph

import (
	"fmt"
	"sync"

	"github.com/wyolet/relay/sdk/catalog"
)

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
