package modeldir

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/wyolet/relay/sdk/catalog"
)

// AddOptions tunes Add.
type AddOptions struct {
	// PricedBy names the host whose rate sheet the file carries. Empty picks the model author's own host when it serves the model, else the first featured host, else the first host; a ref pinned with @host picks that host.
	PricedBy string
	// Overwrite replaces an existing file instead of failing.
	Overwrite bool
}

// errNotInCatalog marks a model (or model at a host) the source catalog does not serve.
var errNotInCatalog = errors.New("not in catalog")

// Add resolves ref (slug, wire name or alias, optionally provider/ or @host qualified) in src and writes <name>.yaml into dir, creating dir if needed. An existing file is an error wrapping fs.ErrExist unless opts.Overwrite is set.
func Add(ctx context.Context, dir, ref string, src Source, opts AddOptions) (Model, error) {
	ic, err := src.Catalog(ctx)
	if err != nil {
		return Model{}, fmt.Errorf("modeldir: source: %w", err)
	}
	slug, err := ic.ResolveModelSlug(ref)
	if err != nil {
		return Model{}, fmt.Errorf("modeldir: %w", err)
	}
	pricedBy := opts.PricedBy
	if pricedBy == "" && strings.ContainsRune(ref, '@') {
		_, h, err := ic.Resolve(ref)
		if err != nil {
			return Model{}, fmt.Errorf("modeldir: %w", err)
		}
		pricedBy = h.Name
	}
	m, err := deriveModel(ic, slug, pricedBy)
	if err != nil {
		return Model{}, err
	}
	if !opts.Overwrite {
		if _, err := os.Stat(modelPath(dir, m.Name)); err == nil {
			return Model{}, fmt.Errorf("modeldir: %s: %w", modelPath(dir, m.Name), fs.ErrExist)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Model{}, fmt.Errorf("modeldir: %w", err)
	}
	if err := writeModelFile(dir, m); err != nil {
		return Model{}, err
	}
	return m, nil
}

// deriveModel builds the file for slug from the catalog, priced by pricedBy or by the default host choice when empty.
func deriveModel(ic *catalog.IndexedCatalog, slug, pricedBy string) (Model, error) {
	info := modelInfo(ic.Catalog, slug)
	b, h, err := pickBinding(ic.Catalog, slug, pricedBy, info.Provider)
	if err != nil {
		return Model{}, err
	}
	return modelFromCatalog(b, h, info, ic.Catalog.Version), nil
}

func modelInfo(c *catalog.Catalog, slug string) catalog.ModelInfo {
	for _, m := range c.Models {
		if m.MetadataName == slug {
			return m
		}
	}
	return catalog.ModelInfo{MetadataName: slug}
}

// pickBinding chooses the binding whose pricing a file carries. The author's own host comes first because its rate is the reference price; routing across hosts is the relay's job, not the file's.
func pickBinding(c *catalog.Catalog, slug, pricedBy, author string) (catalog.Binding, catalog.Host, error) {
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
		return catalog.Binding{}, catalog.Host{}, fmt.Errorf("modeldir: model %q: %w", slug, errNotInCatalog)
	}
	if pricedBy != "" {
		for _, s := range candidates {
			if s.h.Name == pricedBy {
				return s.b, s.h, nil
			}
		}
		return catalog.Binding{}, catalog.Host{}, fmt.Errorf("modeldir: model %q at host %q: %w", slug, pricedBy, errNotInCatalog)
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
