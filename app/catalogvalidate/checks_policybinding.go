package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/manifest"
)

// checkPolicyBindingRefs validates PolicyBinding outbound refs:
//   - the owner scope (spec.project, else a team, user or system
//     metadata.owner) exists
//   - spec.policy exists and is bindable from that scope
//   - every catalog-held subject exists; only a system binding binds a host,
//     and a user's binding binds only that user
func checkPolicyBindingRefs(g *graph) []Issue {
	var out []Issue
	for _, b := range g.PolicyBindings {
		src := Ref{Kind: "PolicyBinding", Name: b.Metadata.Name}
		scope, name := string(b.Metadata.Owner.Kind), ownerName(b.Metadata.Owner.Name, b.Metadata.Owner.ID)
		if b.Spec.Project != "" {
			scope, name = "project", b.Spec.Project
		}
		switch scope {
		case "project":
			if _, ok := g.Projects[name]; !ok {
				out = append(out, refMissing(src, "spec.project", "Project", name))
			}
		case "team":
			if _, ok := g.Teams[name]; !ok {
				out = append(out, refMissing(src, "metadata.owner", "Team", name))
			}
		case "system":
		case "user":
			// The owner may be named by id and the subject by name, so the server checks they are one user.
			if s := b.Spec.Subjects; name == "" || len(s) != 1 || s[0].Kind != "user" {
				out = append(out, Issue{Severity: SeverityError, Kind: KindInvariant,
					Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "spec.subjects"},
					Message: "a user-owned policy binding has one subject, its owner"})
			}
		default:
			out = append(out, Issue{Severity: SeverityError, Kind: KindIncomplete,
				Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "metadata.owner"},
				Message: "a policy binding names spec.project, or a team, user or system owner"})
		}
		if b.Spec.Policy == "" {
			out = append(out, Issue{Severity: SeverityError, Kind: KindIncomplete,
				Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policy"},
				Message: "policy is required"})
		} else if p, ok := g.Policies[b.Spec.Policy]; !ok {
			out = append(out, refMissing(src, "spec.policy", "Policy", b.Spec.Policy))
		} else {
			out = append(out, checkPolicyBindable(g, src, p, scope, name)...)
		}
		out = append(out, checkPolicySubjects(g, src, scope, b.Spec.Subjects)...)
	}
	return out
}

// checkPolicyBindable mirrors the control-plane rule: a binding uses a policy
// of its own scope, an ancestor of it, the system, or a host (a tier
// preset). A personal policy is bound by its owner's own binding or by a team
// or project binding, where the server checks the owner may manage bindings;
// an ownerless user row is the operator's shared row and stays bindable.
func checkPolicyBindable(g *graph, src Ref, p *manifest.PolicyDTO, scope, scopeName string) []Issue {
	field := Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policy"}
	target := Ref{Kind: "Policy", Name: p.Metadata.Name}
	owner := p.Metadata.Owner
	name := ownerName(owner.Name, owner.ID)
	refuse := func(msg string) []Issue {
		return []Issue{{Severity: SeverityError, Kind: KindInvariant, Source: field, Target: target, Message: msg}}
	}
	switch owner.Kind {
	case "project":
		if scope != "project" || name != scopeName {
			return refuse(fmt.Sprintf("policy %q belongs to project %q, outside this binding's %s scope", p.Metadata.Name, name, scope))
		}
	case "team":
		inTeam := scope == "team" && scopeName == name
		if pr, ok := g.Projects[scopeName]; scope == "project" && ok && pr.Spec.Team == name {
			inTeam = true
		}
		if !inTeam {
			return refuse(fmt.Sprintf("policy %q belongs to team %q, outside this binding's %s scope", p.Metadata.Name, name, scope))
		}
	case "user":
		switch {
		case name == "", scope == "team", scope == "project":
		case scope == "user" && scopeName == name:
		default:
			return refuse(fmt.Sprintf("policy %q is personal: its owner binds it to themselves, or a team or project binds it", p.Metadata.Name))
		}
	}
	return nil
}

// checkPolicySubjects validates the subjects the catalog can hold. Users and
// groups name rows outside it and are unchecked.
func checkPolicySubjects(g *graph, src Ref, scope string, subjects []manifest.PolicySubjectDTO) []Issue {
	var out []Issue
	for _, sub := range subjects {
		var exists bool
		kind := ""
		switch sub.Kind {
		case "serviceaccount":
			_, exists = g.ServiceAccounts[sub.Name]
			kind = "ServiceAccount"
		case "host":
			_, exists = g.Hosts[sub.Name]
			kind = "Host"
			if scope != "system" {
				out = append(out, Issue{Severity: SeverityError, Kind: KindInvariant,
					Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "spec.subjects"},
					Target:  Ref{Kind: "Host", Name: sub.Name},
					Message: fmt.Sprintf("host %q is bound only by a system-owned binding", sub.Name)})
			}
		case "account":
			_, exists = g.Accounts[sub.Name]
			kind = "Account"
		case "credential":
			_, exists = g.HostKeys[sub.Name]
			kind = "HostKey"
		case "team":
			_, exists = g.Teams[sub.Name]
			kind = "Team"
		case "project":
			_, exists = g.Projects[sub.Name]
			kind = "Project"
		default:
			continue
		}
		if !exists {
			out = append(out, refMissing(src, "spec.subjects", kind, sub.Name))
		}
	}
	return out
}
