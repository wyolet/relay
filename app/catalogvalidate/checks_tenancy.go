package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/manifest"
)

// checkProjectRefs validates Project outbound refs:
//   - spec.team → Team name must exist
func checkProjectRefs(g *graph) []Issue {
	var out []Issue
	for _, p := range g.Projects {
		src := Ref{Kind: "Project", Name: p.Metadata.Name}
		if p.Spec.Team == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.team"},
				Message:  "team is required",
			})
			continue
		}
		if _, ok := g.Teams[p.Spec.Team]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.team"},
				Target:   Ref{Kind: "Team", Name: p.Spec.Team},
				Message:  fmt.Sprintf("team %q not found", p.Spec.Team),
			})
		}
	}
	return out
}

// checkOwnerProject validates the owner ref of a row that lives inside a
// Project. Rows in any other scope produce no issue.
func checkOwnerProject(g *graph, kind, name string, owner manifest.WireOwner) []Issue {
	if owner.Kind != "project" {
		return nil
	}
	pname := owner.Name
	if pname == "" {
		pname = owner.ID
	}
	if pname == "" {
		return nil
	}
	if _, ok := g.Projects[pname]; ok {
		return nil
	}
	return []Issue{{
		Severity: SeverityError,
		Kind:     KindRefMissing,
		Source:   Ref{Kind: kind, Name: name, Field: "metadata.owner"},
		Target:   Ref{Kind: "Project", Name: pname},
		Message:  fmt.Sprintf("owner project %q not found", pname),
	}}
}

// checkServiceAccountRefs validates that a ServiceAccount's project exists.
func checkServiceAccountRefs(g *graph) []Issue {
	var out []Issue
	for _, sa := range g.ServiceAccounts {
		src := Ref{Kind: "ServiceAccount", Name: sa.Metadata.Name}
		if sa.Spec.Project == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.project"},
				Message:  "project is required",
			})
		} else if _, ok := g.Projects[sa.Spec.Project]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.project"},
				Target:   Ref{Kind: "Project", Name: sa.Spec.Project},
				Message:  fmt.Sprintf("project %q not found", sa.Spec.Project),
			})
		}
	}
	return out
}

// checkKeyRefs validates that a Key's service-account principal exists;
// user principals are unchecked (users are not catalog documents).
func checkKeyRefs(g *graph) []Issue {
	var out []Issue
	for _, rk := range g.Keys {
		src := Ref{Kind: "Key", Name: rk.Metadata.Name}
		if rk.Spec.Principal.Kind == "serviceaccount" {
			if _, ok := g.ServiceAccounts[rk.Spec.Principal.Name]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.principal"},
					Target:   Ref{Kind: "ServiceAccount", Name: rk.Spec.Principal.Name},
					Message:  fmt.Sprintf("service account %q not found", rk.Spec.Principal.Name),
				})
			}
		}
	}
	return out
}
