package catalog

import (
	"fmt"
	"sort"
	"strings"
)

// IndexedCatalog is a loaded catalog with O(1) resolution maps.
type IndexedCatalog struct {
	Catalog *Catalog

	bare      map[string][]loc
	qualified map[string][]loc
	pinned    map[string]loc
}

type loc struct {
	host, binding int
}

func indexCatalog(c *Catalog) (*IndexedCatalog, error) {
	if c == nil {
		return nil, fmt.Errorf("catalog: nil catalog")
	}
	ic := &IndexedCatalog{
		Catalog:   c,
		bare:      map[string][]loc{},
		qualified: map[string][]loc{},
		pinned:    map[string]loc{},
	}
	for hi, h := range c.Hosts {
		for bi, b := range h.Models {
			ic.addBareQualified(hi, bi, b)
			pinKey := normRef(b.Model + "@" + h.Name)
			if pinKey == "" {
				continue
			}
			ic.pinned[pinKey] = loc{hi, bi}
			for _, p := range b.Providers {
				if q := normRef(p + "/" + b.Model + "@" + h.Name); q != "" {
					ic.pinned[q] = loc{hi, bi}
				}
			}
		}
	}
	return ic, nil
}

func (ic *IndexedCatalog) addBareQualified(hi, bi int, b Binding) {
	if k := normRef(b.Model); k != "" {
		ic.bare[k] = append(ic.bare[k], loc{hi, bi})
	}
	for _, p := range b.Providers {
		if q := normRef(p + "/" + b.Model); q != "" {
			ic.qualified[q] = append(ic.qualified[q], loc{hi, bi})
		}
	}
}

// Resolve maps a model ref to its binding and host. Ref forms: bare snapshot
// name, provider/model, or model@host (and provider/model@host). Ambiguous
// bare or provider-qualified refs across multiple hosts return an error listing
// candidate host@model pins.
func (ic *IndexedCatalog) Resolve(ref string) (Binding, Host, error) {
	key := normRef(ref)
	if key == "" {
		return Binding{}, Host{}, fmt.Errorf("catalog: invalid model ref %q", ref)
	}
	if l, ok := ic.pinned[key]; ok {
		return ic.at(l)
	}
	if locs, ok := ic.qualified[key]; ok {
		return ic.pick(key, locs)
	}
	if locs, ok := ic.bare[key]; ok {
		return ic.pick(key, locs)
	}
	return Binding{}, Host{}, fmt.Errorf("catalog: model %q not found", ref)
}

func (ic *IndexedCatalog) pick(key string, locs []loc) (Binding, Host, error) {
	switch len(locs) {
	case 0:
		return Binding{}, Host{}, fmt.Errorf("catalog: model %q not found", key)
	case 1:
		return ic.at(locs[0])
	default:
		return Binding{}, Host{}, fmt.Errorf("catalog: model %q is ambiguous across hosts (%s); pin with @host", key, ic.candidates(locs))
	}
}

func (ic *IndexedCatalog) at(l loc) (Binding, Host, error) {
	if l.host < 0 || l.host >= len(ic.Catalog.Hosts) {
		return Binding{}, Host{}, fmt.Errorf("catalog: internal host index %d", l.host)
	}
	h := ic.Catalog.Hosts[l.host]
	if l.binding < 0 || l.binding >= len(h.Models) {
		return Binding{}, Host{}, fmt.Errorf("catalog: internal binding index %d", l.binding)
	}
	return h.Models[l.binding], h, nil
}

func (ic *IndexedCatalog) candidates(locs []loc) string {
	seen := make(map[string]struct{}, len(locs))
	var out []string
	for _, l := range locs {
		h := ic.Catalog.Hosts[l.host]
		b := h.Models[l.binding]
		s := b.Model + "@" + h.Name
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
