package client

import (
	"context"
	"sync"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/catalogsource"
)

// WithCatalogSource sets where a client that doesn't go through relay reads the public catalog for Models and Catalog. Without it every such client shares one catalogsource.Latest().
func WithCatalogSource(s *catalogsource.LatestSource) Option {
	return func(c *Client) { c.catalogSource = s }
}

var defaultCatalogSource = sync.OnceValue(func() *catalogsource.LatestSource { return catalogsource.Latest() })

// publicCatalog is Catalog for a client that doesn't go through relay. The relay key of a telemetry client authenticates its reports only, so nothing here reaches relay.
func (c *Client) publicCatalog(ctx context.Context) (*ModelCatalog, error) {
	src := c.catalogSource
	if src == nil {
		src = defaultCatalogSource()
	}
	res, err := src.Fetch(ctx)
	if err != nil {
		return nil, err
	}
	ic := res.Catalog
	if c.target.host != "" {
		if ic, err = hostCatalog(ic.Catalog, c.target.host); err != nil {
			return nil, err
		}
	}
	origin := OriginCatalog
	if res.Embedded {
		origin = OriginEmbedded
	}
	return &ModelCatalog{IndexedCatalog: ic, Origin: origin, Stale: res.Stale, Err: res.Err}, nil
}

// publicModels is Models for a client that doesn't go through relay: the binding slugs of its catalog, once each, in catalog order.
func (c *Client) publicModels(ctx context.Context) (*ModelList, error) {
	mc, err := c.publicCatalog(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, h := range mc.Catalog.Hosts {
		for _, b := range h.Models {
			if !seen[b.MetadataName] {
				seen[b.MetadataName] = true
				ids = append(ids, b.MetadataName)
			}
		}
	}
	return &ModelList{IDs: ids, Origin: mc.Origin, Stale: mc.Stale, Err: mc.Err}, nil
}

// hostCatalog is full narrowed to host: that host, the model sidecars its bindings reference, and the providers those name. full is shared, so its slices are copied, never edited.
func hostCatalog(full *catalog.Catalog, host string) (*catalog.IndexedCatalog, error) {
	out := &catalog.Catalog{Version: full.Version, GeneratedAt: full.GeneratedAt, Hosts: []catalog.Host{}}
	models, providers := map[string]bool{}, map[string]bool{}
	for _, h := range full.Hosts {
		if h.Name != host {
			continue
		}
		out.Hosts = append(out.Hosts, h)
		for _, b := range h.Models {
			models[b.MetadataName] = true
			for _, p := range b.Providers {
				providers[p] = true
			}
		}
	}
	for _, m := range full.Models {
		if models[m.MetadataName] {
			out.Models = append(out.Models, m)
			if m.Provider != "" {
				providers[m.Provider] = true
			}
		}
	}
	for _, p := range full.Providers {
		if providers[p.Name] {
			out.Providers = append(out.Providers, p)
		}
	}
	return catalog.Index(out)
}
