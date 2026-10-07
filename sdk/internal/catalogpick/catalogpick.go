// Package catalogpick holds the rule for which host's binding stands for a model when the caller names no host, shared by modeldir (the rate sheet a file carries) and modelroutes (routes that leave host selection to a relay). Resolution of refs to slugs stays in sdk/catalog.
package catalogpick

import (
	"errors"
	"fmt"

	"github.com/wyolet/relay/sdk/catalog"
)

// ErrNotInCatalog marks a model (or model at a host) the catalog does not serve.
var ErrNotInCatalog = errors.New("not in catalog")

// ModelInfo returns the metadata for slug, or a value carrying only the slug when the catalog has none.
func ModelInfo(c *catalog.Catalog, slug string) catalog.ModelInfo {
	for _, m := range c.Models {
		if m.MetadataName == slug {
			return m
		}
	}
	return catalog.ModelInfo{MetadataName: slug}
}

// Binding chooses slug's binding at host, or with host empty: the author's own host, else the first featured host, else the first host. The author's host comes first because its rate is the reference price; routing across hosts is a relay's job. author empty falls back to the first binding's provider.
func Binding(c *catalog.Catalog, slug, host, author string) (catalog.Binding, catalog.Host, error) {
	type served struct {
		b catalog.Binding
		h catalog.Host
	}
	var candidates []served
	for _, h := range c.Hosts {
		for _, b := range h.Models {
			if b.MetadataName == slug {
				candidates = append(candidates, served{b, h})
			}
		}
	}
	if len(candidates) == 0 {
		return catalog.Binding{}, catalog.Host{}, fmt.Errorf("model %q: %w", slug, ErrNotInCatalog)
	}
	if host != "" {
		for _, s := range candidates {
			if s.h.Name == host {
				return s.b, s.h, nil
			}
		}
		return catalog.Binding{}, catalog.Host{}, fmt.Errorf("model %q at host %q: %w", slug, host, ErrNotInCatalog)
	}
	if author == "" && len(candidates[0].b.Providers) > 0 {
		author = candidates[0].b.Providers[0]
	}
	for _, s := range candidates {
		if s.h.Name == author {
			return s.b, s.h, nil
		}
	}
	for _, s := range candidates {
		if s.b.Featured {
			return s.b, s.h, nil
		}
	}
	return candidates[0].b, candidates[0].h, nil
}
