package catalogvalidate

import (
	"fmt"
	"strings"
)

// checkDuplicateNames emits an error for every name that appeared more
// than once within a kind. The graph index already silently overwrote
// later entries; this surfaces the conflict.
func checkDuplicateNames(g *graph) []Issue {
	out := make([]Issue, 0, len(g.duplicates))
	for _, dup := range g.duplicates {
		out = append(out, Issue{
			Severity: SeverityError,
			Kind:     KindDuplicateName,
			Source:   dup,
			Message:  fmt.Sprintf("more than one %s named %q in the catalog", dup.Kind, dup.Name),
		})
	}
	return out
}

// checkProviderRefs validates Provider-side cross-refs.
// Today providers carry no outbound refs (owner is system by default).
// Kept as a stub so adding refs later (e.g. parent-org) is one place.
func checkProviderRefs(_ *graph) []Issue { return nil }

// checkHostRefs validates Host outbound refs. Hosts carry none today; a
// host's tier policies point at it through their owner instead.
func checkHostRefs(_ *graph) []Issue { return nil }

// checkModelRefs validates Model outbound refs and intra-model invariants:
//   - metadata.owner.id / owner.name → Provider must exist when owner.kind = provider
//   - spec.snapshots[] must be non-empty (otherwise nothing addressable)
func checkModelRefs(g *graph) []Issue {
	var out []Issue
	for _, m := range g.Models {
		src := Ref{Kind: "Model", Name: m.Metadata.Name}

		// Owner ref (Provider).
		if m.Metadata.Owner.Kind == "provider" {
			pname := m.Metadata.Owner.Name
			if pname == "" {
				pname = m.Metadata.Owner.ID
			}
			if pname != "" {
				if _, ok := g.Providers[pname]; !ok {
					out = append(out, Issue{
						Severity: SeverityError,
						Kind:     KindRefMissing,
						Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "metadata.owner"},
						Target:   Ref{Kind: "Provider", Name: pname},
						Message:  fmt.Sprintf("owner provider %q not found", pname),
					})
				}
			}
		}

		// Snapshots presence.
		if len(m.Spec.Snapshots) == 0 {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.snapshots"},
				Message:  "model has no snapshots; nothing is customer-addressable",
			})
		}
	}
	return out
}

// refMissing reports one name a field references that the catalog lacks.
func refMissing(src Ref, field, kind, name string) Issue {
	return Issue{
		Severity: SeverityError,
		Kind:     KindRefMissing,
		Source:   Ref{Kind: src.Kind, Name: src.Name, Field: field},
		Target:   Ref{Kind: kind, Name: name},
		Message:  fmt.Sprintf("%s%s %q not found", strings.ToLower(kind[:1]), kind[1:], name),
	}
}

// ownerName is the name (or id) a wire owner carries.
func ownerName(name, id string) string {
	if name != "" {
		return name
	}
	return id
}
