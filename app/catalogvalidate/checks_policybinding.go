package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/manifest"
)

// checkPolicyBindingRefs validates PolicyBinding outbound refs:
//   - the owner scope (a project, team, user or system metadata.owner)
//     exists
//   - spec.policy exists and is bindable from that scope
//   - spec.accounts names at least one account; a provider account the
//     catalog holds must exist, the others (made by relay with their team,
//     project, user or service account) are left to the server
func checkPolicyBindingRefs(g *graph) []Issue {
	var out []Issue
	for _, b := range g.PolicyBindings {
		src := Ref{Kind: "PolicyBinding", Name: b.Metadata.Name}
		scope, name := string(b.Metadata.Owner.Kind), ownerName(b.Metadata.Owner.Name, b.Metadata.Owner.ID)
		switch scope {
		case "project":
			if _, ok := g.Projects[name]; !ok {
				out = append(out, refMissing(src, "metadata.owner", "Project", name))
			}
		case "team":
			if _, ok := g.Teams[name]; !ok {
				out = append(out, refMissing(src, "metadata.owner", "Team", name))
			}
		case "system", "user":
		default:
			out = append(out, Issue{Severity: SeverityError, Kind: KindIncomplete,
				Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "metadata.owner"},
				Message: "a policy binding is owned by a project, a team, a user or the system"})
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
		if len(b.Spec.Accounts) == 0 {
			out = append(out, Issue{Severity: SeverityError, Kind: KindIncomplete,
				Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "spec.accounts"},
				Message: "a policy binding binds at least one account"})
		}
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
			return refuse(fmt.Sprintf("policy %q is personal: its owner binds it to their own accounts, or a team or project binds it", p.Metadata.Name))
		}
	}
	return nil
}
