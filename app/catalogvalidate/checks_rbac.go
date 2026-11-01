package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/manifest"
)

// checkRoleBindingRefs validates RoleBinding outbound refs:
//   - spec.role → Role name must exist
//   - spec.scope → the named Team or Project must exist
//   - service-account subjects must exist (users and groups are not catalog
//     documents, so they are unchecked)
func checkRoleBindingRefs(g *graph) []Issue {
	var out []Issue
	for _, b := range g.RoleBindings {
		src := Ref{Kind: "RoleBinding", Name: b.Metadata.Name}
		if b.Spec.Role == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.role"},
				Message:  "role is required",
			})
		} else if _, ok := g.Roles[b.Spec.Role]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.role"},
				Target:   Ref{Kind: "Role", Name: b.Spec.Role},
				Message:  fmt.Sprintf("role %q not found", b.Spec.Role),
			})
		}
		out = append(out, checkScopeTarget(g, src, b.Spec.Scope)...)
		out = append(out, checkSubjects(g, src, b.Spec.Subjects)...)
	}
	return out
}

// checkScopeTarget validates the team/project a binding is scoped to. The
// system scope names nothing.
func checkScopeTarget(g *graph, src Ref, scope manifest.WireOwner) []Issue {
	field := Ref{Kind: src.Kind, Name: src.Name, Field: "spec.scope"}
	name := scope.Name
	if name == "" {
		name = scope.ID
	}
	switch scope.Kind {
	case "team":
		if _, ok := g.Teams[name]; !ok {
			return []Issue{{Severity: SeverityError, Kind: KindRefMissing, Source: field,
				Target:  Ref{Kind: "Team", Name: name},
				Message: fmt.Sprintf("team %q not found", name)}}
		}
	case "project":
		if _, ok := g.Projects[name]; !ok {
			return []Issue{{Severity: SeverityError, Kind: KindRefMissing, Source: field,
				Target:  Ref{Kind: "Project", Name: name},
				Message: fmt.Sprintf("project %q not found", name)}}
		}
	case "system", "":
	default:
		return []Issue{{Severity: SeverityError, Kind: KindInvariant, Source: field,
			Message: fmt.Sprintf("scope.kind must be system, team, or project, got %q", scope.Kind)}}
	}
	return nil
}

// checkSubjects validates the service-account subjects of a binding. User
// and group subjects name rows that live outside the catalog.
func checkSubjects(g *graph, src Ref, subjects []manifest.SubjectDTO) []Issue {
	var out []Issue
	for _, sub := range subjects {
		if sub.Kind != "serviceaccount" {
			continue
		}
		if _, ok := g.ServiceAccounts[sub.Name]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.subjects"},
				Target:   Ref{Kind: "ServiceAccount", Name: sub.Name},
				Message:  fmt.Sprintf("service account %q not found", sub.Name),
			})
		}
	}
	return out
}
