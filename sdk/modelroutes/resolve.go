package modelroutes

import (
	"errors"
	"fmt"

	"github.com/wyolet/relay/sdk/catalog"
	"github.com/wyolet/relay/sdk/internal/catalogpick"
)

// Entry is one listed model joined with its route and catalog data.
type Entry struct {
	// Key is the [models] key verbatim, the picker id.
	Key string
	// Model is the catalog ref as written.
	Model string
	// Cataloged is false only for a model a passthrough route carries that the catalog does not list; Binding and Info are then zero.
	Cataloged bool
	Route     Route
	Binding   catalog.Binding
	// Host is the binding's host, or for an uncataloged model the route's catalog host, if any.
	Host catalog.Host
	Info catalog.ModelInfo
}

// Endpoint is the adapter name, base URL and request path to call: the route's overrides, else the binding's adapter and the host's base URL and path. path "" means the adapter's default path. It is always "" when the route sets url, since the host's path describes the catalog host's layout, not that server's. A host whose catalog path is an explicit "" (its base URL is the complete endpoint) also yields ""; Host.Path tells the two apart.
func (e Entry) Endpoint() (adapter, baseURL, path string) {
	adapter, baseURL = e.Route.Adapter, e.Route.URL
	if adapter == "" {
		adapter = e.Binding.Adapter
	}
	if baseURL == "" {
		baseURL = e.Host.BaseURL
		if e.Host.Path != nil {
			path = *e.Host.Path
		}
	}
	return adapter, baseURL, path
}

// WireName is the model name to send upstream: the binding's wire name, or for an uncataloged model the ref verbatim.
func (e Entry) WireName() string {
	if e.Cataloged {
		return e.Binding.Name
	}
	return e.Model
}

// Resolve joins every listed model with its route and the catalog, in file order. Every problem in the file is reported in one error, line-numbered when f came from Parse or Read.
func Resolve(f *File, ic *catalog.IndexedCatalog) ([]Entry, error) {
	var p problems
	if f.Catalog == "" {
		p.add(f, position{name: "catalog"}, "catalog is empty; pin a relay-catalog release tag")
	}
	routes, usable := checkRoutes(f, ic, &p)
	keys := map[string]bool{}
	var entries []Entry
	for _, m := range f.Models {
		if keys[m.Key] {
			p.add(f, position{"models", m.Key}, "%s: duplicate key", describe(m))
			continue
		}
		keys[m.Key] = true
		r, ok := routes[m.Route]
		if !ok {
			p.add(f, position{"models", m.Key}, "%s: unknown route %q", describe(m), m.Route)
			continue
		}
		if !usable[m.Route] {
			continue
		}
		e, err := resolveEntry(ic, m, r)
		if err != nil {
			p.add(f, position{"models", m.Key}, "%s: %v", describe(m), err)
			continue
		}
		entries = append(entries, e)
	}
	if f.Default != "" && !keys[f.Default] {
		p.add(f, position{name: "default"}, "default %q is not a [models] key", f.Default)
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// checkRoutes indexes f's routes by name and reports which are usable; a model on an unusable route is not checked further, since its route's problem is already reported.
func checkRoutes(f *File, ic *catalog.IndexedCatalog, p *problems) (map[string]Route, map[string]bool) {
	routes := map[string]Route{}
	usable := map[string]bool{}
	for _, r := range f.Routes {
		at := position{"routes", r.Name}
		if _, dup := routes[r.Name]; dup {
			p.add(f, at, "route %q: duplicate", r.Name)
			continue
		}
		routes[r.Name] = r
		errs := routeProblems(r, ic)
		for _, err := range errs {
			p.add(f, at, "route %q: %v", r.Name, err)
		}
		usable[r.Name] = len(errs) == 0
	}
	return routes, usable
}

func routeProblems(r Route, ic *catalog.IndexedCatalog) []error {
	var errs []error
	if r.Host == "" && r.URL == "" {
		errs = append(errs, errors.New("needs a host or a url"))
	}
	if r.Host != "" {
		if _, ok := catalogHost(ic, r.Host); !ok {
			errs = append(errs, fmt.Errorf("unknown catalog host %q", r.Host))
		}
	}
	switch r.Auth {
	case "", "key", "subscription", "none":
	default:
		errs = append(errs, fmt.Errorf("auth %q: want key, subscription or none", r.Auth))
	}
	if r.Passthrough && r.Adapter == "" {
		errs = append(errs, errors.New("passthrough needs an adapter, since an uncataloged model has no binding to supply one"))
	}
	return errs
}

func catalogHost(ic *catalog.IndexedCatalog, name string) (catalog.Host, bool) {
	for _, h := range ic.Catalog.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return catalog.Host{}, false
}

// resolveEntry joins one model with a usable route. A route host pins the binding; without one the catalog's default binding stands in, the same choice modeldir makes, because a relay picks the real host. Only a not-found ref counts as absent (and passes through on a passthrough route); an invalid or ambiguous ref is reported as is.
func resolveEntry(ic *catalog.IndexedCatalog, m ModelRoute, r Route) (Entry, error) {
	model := m.Model
	if model == "" {
		model = m.Key
	}
	e := Entry{Key: m.Key, Model: model, Cataloged: true, Route: r}
	if r.Host != "" {
		b, h, err := ic.Resolve(model + "@" + r.Host)
		switch {
		case err == nil:
			e.Binding, e.Host, e.Info = b, h, catalogpick.ModelInfo(ic.Catalog, b.MetadataName)
			return e, nil
		case !errors.Is(err, catalog.ErrNotFound):
			return Entry{}, err
		case !r.Passthrough:
			return Entry{}, fmt.Errorf("not served on host %q", r.Host)
		}
		e.Cataloged = false
		e.Host, _ = catalogHost(ic, r.Host)
		return e, nil
	}
	slug, err := ic.ResolveModelSlug(model)
	switch {
	case err == nil:
		info := catalogpick.ModelInfo(ic.Catalog, slug)
		b, h, err := catalogpick.Binding(ic.Catalog, slug, "", info.Provider)
		if err != nil {
			return Entry{}, err
		}
		e.Binding, e.Host, e.Info = b, h, info
		return e, nil
	case !errors.Is(err, catalog.ErrNotFound):
		return Entry{}, err
	case !r.Passthrough:
		return Entry{}, errors.New("not in the catalog")
	}
	e.Cataloged = false
	return e, nil
}

func describe(m ModelRoute) string {
	if m.Model == "" || m.Model == m.Key {
		return fmt.Sprintf("model %q", m.Key)
	}
	return fmt.Sprintf("%q (model %q)", m.Key, m.Model)
}
