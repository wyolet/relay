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
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   src,
				Target:   Ref{Kind: "Provider", Name: ref.Provider},
				Message:  fmt.Sprintf("modelref %q: provider %q not found", raw, ref.Provider),
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
