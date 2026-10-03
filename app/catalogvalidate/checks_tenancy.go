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

// checkServiceAccountRefs validates ServiceAccount outbound refs:
//   - spec.project → Project name must exist
//   - spec.policy → Policy name must exist when set
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
		if sa.Spec.Policy != "" {
			if p, ok := g.Policies[sa.Spec.Policy]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policy"},
					Target:   Ref{Kind: "Policy", Name: sa.Spec.Policy},
					Message:  fmt.Sprintf("policy %q not found", sa.Spec.Policy),
				})
			} else {
				out = append(out, checkPolicyBindable(src, p, sa.Spec.Project)...)
			}
		}
	}
	return out
}

// checkPolicyBindable mirrors the control-plane rule: only a policy of the
// binder's own project, or a system-owned shared one, may be bound. A
// host-owned policy is an upstream tier definition carrying no inbound
// grants, so binding one resolves a principal to a policy with no keys.
func checkPolicyBindable(src Ref, p *manifest.PolicyDTO, project string) []Issue {
	field := Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policy"}
	target := Ref{Kind: "Policy", Name: p.Metadata.Name}
	owner := p.Metadata.Owner
	switch owner.Kind {
	case "host":
		return []Issue{{Severity: SeverityError, Kind: KindInvariant, Source: field, Target: target,
			Message: fmt.Sprintf("policy %q is a host tier policy and cannot be bound", p.Metadata.Name)}}
	case "project":
		name := owner.Name
		if name == "" {
			name = owner.ID
		}
		if name != project {
			return []Issue{{Severity: SeverityError, Kind: KindInvariant, Source: field, Target: target,
				Message: fmt.Sprintf("policy %q belongs to project %q, not %q", p.Metadata.Name, name, project)}}
		}
	case "user":
		// An ownerless user row is the operator's shared row and stays
		// bindable; one naming a person is that person's alone.
		if owner.Name != "" || owner.ID != "" {
			return []Issue{{Severity: SeverityError, Kind: KindInvariant, Source: field, Target: target,
				Message: fmt.Sprintf("policy %q is personal and cannot be bound from a project", p.Metadata.Name)}}
		}
	}
	return nil
}

// checkKeyRefs validates Key outbound refs:
//   - spec.principal → a ServiceAccount must exist (user principals are
//     unchecked: users are not catalog documents)
//   - spec.policy → Policy name must exist
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
		if rk.Spec.Policy == "" {
			continue
		}
		if _, ok := g.Policies[rk.Spec.Policy]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policy"},
				Target:   Ref{Kind: "Policy", Name: rk.Spec.Policy},
				Message:  fmt.Sprintf("policy %q not found", rk.Spec.Policy),
			})
		}
	}
	return out
}
