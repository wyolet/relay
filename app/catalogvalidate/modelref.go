package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/modelref"
)

// validateModelRef parses one policy.spec.models[i] DSL string and checks
// any explicit provider/host names mentioned actually exist. Wildcard
// segments are not validated. Parse errors are not surfaced — the
// per-entity Validate() catches malformed refs already; here we only
// pursue cross-graph resolution.
func validateModelRef(raw, policyName string, index int, g *graph) []Issue {
	ref, err := modelref.Parse(raw)
	if err != nil {
		return nil
	}
	src := Ref{
		Kind:  "Policy",
		Name:  policyName,
		Field: fmt.Sprintf("spec.models[%d]", index),
	}
	var out []Issue

	// Explicit provider slug must resolve.
	if ref.Provider != "" && !ref.ProviderWildcard {
		if _, ok := g.Providers[ref.Provider]; !ok {
			msg := fmt.Sprintf("modelref %q: provider %q not found", raw, ref.Provider)
			// The grammar reads a lone segment as a provider, so a bare model
			// name matches nothing at routing time; name the actual mistake.
			if owner, isModel := modelOwner(g, ref.Provider); ref.ModelWildcard && isModel {
				msg = fmt.Sprintf("modelref %q is a bare model name; entries need a provider segment", raw)
				if owner != "" {
					msg += fmt.Sprintf(" (%q)", owner+"/"+raw)
				}
			}
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   src,
				Target:   Ref{Kind: "Provider", Name: ref.Provider},
				Message:  msg,
			})
		}
	}
	// Explicit host slug must resolve.
	if ref.Host != "" && !ref.HostWildcard {
		if _, ok := g.Hosts[ref.Host]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   src,
				Target:   Ref{Kind: "Host", Name: ref.Host},
				Message:  fmt.Sprintf("modelref %q: host %q not found", raw, ref.Host),
			})
		}
	}
	// Explicit model slug must resolve.
	if ref.Model != "" && !ref.ModelWildcard {
		if _, ok := g.Models[ref.Model]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   src,
				Target:   Ref{Kind: "Model", Name: ref.Model},
				Message:  fmt.Sprintf("modelref %q: model %q not found", raw, ref.Model),
			})
		}
	}
	return out
}

// modelOwner reports whether slug names a Model or one of its snapshots,
// and that model's owning provider name ("" when the owner isn't a provider).
func modelOwner(g *graph, slug string) (string, bool) {
	for name, m := range g.Models {
		match := name == slug
		for i := 0; !match && i < len(m.Spec.Snapshots); i++ {
			match = m.Spec.Snapshots[i].Name == slug
		}
		if !match {
			continue
		}
		if m.Metadata.Owner.Kind == "provider" {
			return m.Metadata.Owner.Name, true
		}
		return "", true
	}
	return "", false
}
