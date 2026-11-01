package catalogvalidate

import "fmt"

// checkHostKeyRefs validates HostKey outbound refs:
//   - spec.hostId → Host name must exist
//   - spec.account → Account name must exist and be held at the same host
func checkHostKeyRefs(g *graph) []Issue {
	var out []Issue
	for _, hk := range g.HostKeys {
		src := Ref{Kind: "HostKey", Name: hk.Metadata.Name}
		out = append(out, checkOwnerProject(g, src.Kind, src.Name, hk.Metadata.Owner)...)

		switch _, ok := g.Hosts[hk.Spec.HostID]; {
		case hk.Spec.HostID == "":
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.hostId"},
				Message:  "hostId is required",
			})
		case !ok:
			out = append(out, refMissing(src, "spec.hostId", "Host", hk.Spec.HostID))
		}

		if hk.Spec.Account == "" {
			continue
		}
		a, ok := g.Accounts[hk.Spec.Account]
		if !ok {
			out = append(out, refMissing(src, "spec.account", "Account", hk.Spec.Account))
			continue
		}
		if a.Spec.Host != hk.Spec.HostID {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindOwnerMismatch,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.account"},
				Target:   Ref{Kind: "Account", Name: hk.Spec.Account},
				Message:  fmt.Sprintf("hostkey on host %q names account %q held at host %q", hk.Spec.HostID, hk.Spec.Account, a.Spec.Host),
			})
		}
	}
	return out
}

// checkAccountRefs validates that every Account's host exists.
func checkAccountRefs(g *graph) []Issue {
	var out []Issue
	for _, a := range g.Accounts {
		src := Ref{Kind: "Account", Name: a.Metadata.Name}
		out = append(out, checkOwnerProject(g, src.Kind, src.Name, a.Metadata.Owner)...)
		if _, ok := g.Hosts[a.Spec.Host]; !ok {
			out = append(out, refMissing(src, "spec.host", "Host", a.Spec.Host))
		}
	}
	return out
}

// checkPoolRefs validates a Pool's host and its credentials, each of which
// must authenticate to the pool's host.
func checkPoolRefs(g *graph) []Issue {
	var out []Issue
	for _, p := range g.Pools {
		src := Ref{Kind: "Pool", Name: p.Metadata.Name}
		out = append(out, checkOwnerProject(g, src.Kind, src.Name, p.Metadata.Owner)...)
		if _, ok := g.Hosts[p.Spec.Host]; !ok {
			out = append(out, refMissing(src, "spec.host", "Host", p.Spec.Host))
		}
		for i, name := range p.Spec.Credentials {
			field := fmt.Sprintf("spec.credentials[%d]", i)
			hk, ok := g.HostKeys[name]
			if !ok {
				out = append(out, refMissing(src, field, "HostKey", name))
				continue
			}
			if hk.Spec.HostID != p.Spec.Host {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindOwnerMismatch,
					Source:   Ref{Kind: src.Kind, Name: src.Name, Field: field},
					Target:   Ref{Kind: "HostKey", Name: name},
					Message:  fmt.Sprintf("hostkey %q authenticates to host %q, not the pool's host %q", name, hk.Spec.HostID, p.Spec.Host),
				})
			}
		}
	}
	return out
}

// checkBudgetRefs validates that a project-owned Budget's project exists.
func checkBudgetRefs(g *graph) []Issue {
	var out []Issue
	for _, b := range g.Budgets {
		out = append(out, checkOwnerProject(g, "Budget", b.Metadata.Name, b.Metadata.Owner)...)
	}
	return out
}
