// modelrefs.go holds the canonical form of a policy's catalog-ref grants.
// Every writer of a Policy row — the control API and apply — must store the
// same string for the same grant, or the two disagree forever: apply would
// report an update on every run against a row the API just normalised.
package policy

import (
	"fmt"

	"github.com/wyolet/relay/app/modelref"
)

// CanonicalizeModelRefs slugifies every ref to its shortest canonical form
// and drops post-slug duplicates, preserving order. Catalog resolution (does
// the ref match an enabled binding?) is a separate, store-backed check the
// caller layers on top.
func CanonicalizeModelRefs(refs []string) ([]string, error) {
	if len(refs) == 0 {
		return refs, nil
	}
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, raw := range refs {
		ref, err := modelref.Parse(raw)
		if err != nil {
			return nil, err
		}
		c := CanonicalModelRef(ref)
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out, nil
}

// CanonicalModelRef renders a parsed Ref back to its shortest slug form. The
// Ref's segments are already slug-normalized by modelref.Parse.
func CanonicalModelRef(r modelref.Ref) string {
	prov, mdl, hst := "", "", ""
	if !r.ProviderWildcard {
		prov = r.Provider
	}
	if !r.ModelWildcard {
		mdl = r.Model
	}
	if !r.HostWildcard {
		hst = r.Host
	}
	s, err := modelref.Format(prov, mdl, hst)
	if err != nil {
		return r.Raw
	}
	return s
}

// CanonicalizeSpecModelRefs rewrites every grant list on the spec in place:
// Spec.Models and each RLBinding's Models. Errors name the offending field.
func (p *Policy) CanonicalizeSpecModelRefs() error {
	norm, err := CanonicalizeModelRefs(p.Spec.Models)
	if err != nil {
		return fmt.Errorf("policy %q: models: %w", p.Meta.Name, err)
	}
	p.Spec.Models = norm
	for i := range p.Spec.RLBindings {
		norm, err := CanonicalizeModelRefs(p.Spec.RLBindings[i].Models)
		if err != nil {
			return fmt.Errorf("policy %q: rlBindings: %w", p.Meta.Name, err)
		}
		p.Spec.RLBindings[i].Models = norm
	}
	return nil
}
