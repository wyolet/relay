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

	// Declared-alias maps, probed only after the real-name maps miss —
	// mirroring the relay's last-priority alias semantics. Patterns are
	// bare-only and sorted longest-prefix-first.
	aliasBare      map[string][]loc
	aliasQualified map[string][]loc
	aliasPinned    map[string]loc
	aliasPatterns  []aliasPattern
}

type loc struct {
	host, binding int
}

type aliasPattern struct {
	prefix, suffix string
	l              loc
}

func indexCatalog(c *Catalog) (*IndexedCatalog, error) {
	if c == nil {
		return nil, fmt.Errorf("catalog: nil catalog")
	}
	ic := &IndexedCatalog{
		Catalog:        c,
		bare:           map[string][]loc{},
		qualified:      map[string][]loc{},
		pinned:         map[string]loc{},
		aliasBare:      map[string][]loc{},
		aliasQualified: map[string][]loc{},
		aliasPinned:    map[string]loc{},
	}
	for hi, h := range c.Hosts {
		for bi, b := range h.Models {
			l := loc{hi, bi}
			ic.addForms(l, b.Model, b.Providers, h.Name)
			if uk := normRef(b.Upstream); uk != "" && uk != normRef(b.Model) {
				ic.addForms(l, b.Upstream, b.Providers, h.Name)
			}
			for _, a := range b.Aliases {
				if pre, post, found := strings.Cut(a, "*"); found {
					if p := normPrefix(pre); p != "" {
						ic.aliasPatterns = append(ic.aliasPatterns, aliasPattern{prefix: p, suffix: normSuffix(post), l: l})
					}
					continue
				}
				ic.addAliasForms(l, a, b.Providers, h.Name)
			}
		}
	}
	// Longest normalized prefix wins on overlap; index-order tiebreak is
	// already deterministic (catalog file order).
	sort.SliceStable(ic.aliasPatterns, func(i, j int) bool {
		return len(ic.aliasPatterns[i].prefix) > len(ic.aliasPatterns[j].prefix)
	})
	return ic, nil
}

// addAliasForms mirrors addForms into the declared-alias maps.
func (ic *IndexedCatalog) addAliasForms(l loc, name string, providers []string, host string) {
	if k := normRef(name); k != "" {
		ic.aliasBare[k] = append(ic.aliasBare[k], l)
	}
	for _, p := range providers {
		if q := normRef(p + "/" + name); q != "" {
			ic.aliasQualified[q] = append(ic.aliasQualified[q], l)
		}
	}
	pinKey := normRef(name + "@" + host)
	if pinKey == "" {
		return
	}
	ic.aliasPinned[pinKey] = l
	for _, p := range providers {
		if q := normRef(p + "/" + name + "@" + host); q != "" {
			ic.aliasPinned[q] = l
		}
	}
}

// addForms indexes every addressable form of one name for a binding: bare,
// provider/name, name@host, provider/name@host. Called once with the catalog
// key (Model) and again with the served wire name (Upstream) when the two
// normalize differently — a response's ran-model is the provider's spelling,
// and it must resolve without the caller keeping a private slug dictionary.
func (ic *IndexedCatalog) addForms(l loc, name string, providers []string, host string) {
	if k := normRef(name); k != "" {
		ic.bare[k] = append(ic.bare[k], l)
	}
	for _, p := range providers {
		if q := normRef(p + "/" + name); q != "" {
			ic.qualified[q] = append(ic.qualified[q], l)
		}
	}
	pinKey := normRef(name + "@" + host)
	if pinKey == "" {
		return
	}
	ic.pinned[pinKey] = l
	for _, p := range providers {
		if q := normRef(p + "/" + name + "@" + host); q != "" {
			ic.pinned[q] = l
		}
	}
}

// Resolve maps a model ref to its binding and host. Ref forms: bare snapshot
// name, provider/model, or model@host (and provider/model@host). The model
// segment accepts either the catalog key (Binding.Model) or the served wire
// name (Binding.Upstream) — the string a provider echoes back as the ran
// model resolves as-is. Ambiguous bare or provider-qualified refs across
// multiple hosts return an error listing candidate host@model pins.
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
	// Declared aliases: last priority, so real catalog names always win.
	if l, ok := ic.aliasPinned[key]; ok {
		return ic.at(l)
	}
	if locs, ok := ic.aliasQualified[key]; ok {
		return ic.pick(key, locs)
	}
	if locs, ok := ic.aliasBare[key]; ok {
		return ic.pick(key, locs)
	}
	// Wildcards are bare-only: a pinned ref ("@host") would glue the pin
	// into the key and corrupt the match, so it skips the pattern scan.
	if !strings.ContainsRune(ref, '@') {
		for _, p := range ic.aliasPatterns {
			if len(key) >= len(p.prefix)+len(p.suffix) &&
				strings.HasPrefix(key, p.prefix) && strings.HasSuffix(key, p.suffix) {
				return ic.at(p.l)
			}
		}
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
