package modelref

import (
	"errors"
	"fmt"
	"sort"
)

// ConcreteBinding is a fully-qualified (provider, model, host) triple.
// Used by Overlap*/Resolve/Assign helpers as the catalog index.
type ConcreteBinding struct {
	Provider string
	Model    string
	Host     string
}

func (b ConcreteBinding) String() string {
	return b.Provider + "/" + b.Model + "@" + b.Host
}

// Validate runs Parse and returns any error.
func Validate(s string) error {
	_, err := Parse(s)
	return err
}

// Format builds the canonical shortest ref form for the given segments.
// Empty string means "wildcard / absent" on that axis.
func Format(provider, model, host string) (string, error) {
	if provider == "" && host == "" {
		return "", errors.New("modelref.Format: at least provider or host required")
	}
	if provider == "" && model != "" {
		return "", errors.New("modelref.Format: model requires provider")
	}
	if provider == "" {
		return "@" + host, nil
	}
	out := provider
	if model != "" {
		out += "/" + model
	}
	if host != "" {
		out += "@" + host
	}
	return out, nil
}

// Covers reports whether r grants binding b. Each defined segment must
// match; wildcard segments pass.
func (r Ref) Covers(b ConcreteBinding) bool {
	return r.Matches(b.Provider, b.Model, b.Host)
}

// Includes reports whether every concrete binding that satisfies inner
// also satisfies outer.
func Includes(outer, inner Ref) bool {
	if !outer.ProviderWildcard {
		if inner.ProviderWildcard || inner.Provider != outer.Provider {
			return false
		}
	}
	if !outer.ModelWildcard {
		if inner.ModelWildcard || inner.Model != outer.Model {
			return false
		}
	}
	if !outer.HostWildcard {
		if inner.HostWildcard || inner.Host != outer.Host {
			return false
		}
	}
	return true
}

// Overlap reports whether some concrete binding could satisfy both refs.
// Per-axis: if both refs name a value, they must agree; wildcards always pass.
func Overlap(a, b Ref) bool {
	if !a.ProviderWildcard && !b.ProviderWildcard && a.Provider != b.Provider {
		return false
	}
	if !a.ModelWildcard && !b.ModelWildcard && a.Model != b.Model {
		return false
	}
	if !a.HostWildcard && !b.HostWildcard && a.Host != b.Host {
		return false
	}
	return true
}

// Specificity scores a ref. Higher = more specific = wins ties when two
// refs both cover the same binding. Host-anchoring beats model-anchoring
// at equal segment counts (host is the credentials/billing boundary).
func Specificity(r Ref) int {
	segs := 0
	if !r.ProviderWildcard {
		segs++
	}
	if !r.ModelWildcard {
		segs++
	}
	if !r.HostWildcard {
		segs++
	}
	score := 10 * segs
	if !r.HostWildcard {
		score += 2
	}
	if !r.ModelWildcard {
		score += 1
	}
	return score
}

// OverlappingBindings returns every binding in catalog covered by BOTH
// refs. Returns nil (not empty) when refs are conceptually disjoint.
func OverlappingBindings(a, b Ref, catalog []ConcreteBinding) []ConcreteBinding {
	if !Overlap(a, b) {
		return nil
	}
	out := make([]ConcreteBinding, 0)
	for _, c := range catalog {
		if a.Covers(c) && b.Covers(c) {
			out = append(out, c)
		}
	}
	return out
}

// Resolve expands each ref against catalog. Map key is ref.Raw; value
// is the bindings that ref covers (in catalog order).
func Resolve(refs []Ref, catalog []ConcreteBinding) map[string][]ConcreteBinding {
	out := make(map[string][]ConcreteBinding, len(refs))
	for _, r := range refs {
		matches := make([]ConcreteBinding, 0)
		for _, c := range catalog {
			if r.Covers(c) {
				matches = append(matches, c)
			}
		}
		out[r.Raw] = matches
	}
	return out
}

// formatRef is the canonical string for a parsed Ref. Used by error
// messages and dedup keys.
func formatRef(r Ref) string {
	s, err := Format(refField(r.Provider, r.ProviderWildcard),
		refField(r.Model, r.ModelWildcard),
		refField(r.Host, r.HostWildcard))
	if err != nil {
		return fmt.Sprintf("<invalid:%s>", r.Raw)
	}
	return s
}

func refField(v string, wildcard bool) string {
	if wildcard {
		return ""
	}
	return v
}

// sortBindings returns a copy of bindings sorted by canonical string,
// useful for stable test output and UI display.
func sortBindings(bs []ConcreteBinding) []ConcreteBinding {
	out := make([]ConcreteBinding, len(bs))
	copy(out, bs)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
