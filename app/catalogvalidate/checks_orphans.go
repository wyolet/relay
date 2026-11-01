package catalogvalidate

// checkOrphans surfaces curation hints (warnings, not errors):
//   - Provider with zero Models
//   - Model with zero enabled host bindings
//   - HostKey not linked by any routable Policy, directly or through a Pool
//   - RateLimit not referenced by any Policy
//
// Errors-by-orphaning would be too strict — operators may legitimately
// stage a Provider before populating its Models, or define a RateLimit
// shared by future Policies.
func checkOrphans(g *graph) []Issue {
	var out []Issue

	// Provider → Model index.
	providerModels := map[string]int{}
	for _, m := range g.Models {
		if m.Metadata.Owner.Kind != "provider" {
			continue
		}
		pname := m.Metadata.Owner.Name
		if pname == "" {
			pname = m.Metadata.Owner.ID
		}
		if pname != "" {
			providerModels[pname]++
		}
	}
	for name := range g.Providers {
		if providerModels[name] == 0 {
			out = append(out, Issue{
				Severity: SeverityWarning,
				Kind:     KindOrphan,
				Source:   Ref{Kind: "Provider", Name: name},
				Message:  "provider has no models",
			})
		}
	}

	// Model → at least one enabled host binding (from standalone HostBinding docs).
	modelHasBinding := map[string]bool{}
	for _, b := range g.HostBindings {
		if b.Spec.Enabled == nil || *b.Spec.Enabled {
			modelHasBinding[b.Spec.Model] = true
		}
	}
	for _, m := range g.Models {
		if modelHasBinding[m.Metadata.Name] {
			continue
		}
		out = append(out, Issue{
			Severity: SeverityWarning,
			Kind:     KindOrphan,
			Source:   Ref{Kind: "Model", Name: m.Metadata.Name},
			Message:  "model has no enabled host bindings; not reachable",
		})
	}

	// HostKey → linked by a caller policy, directly or through a pool. Team-
	// and project-owned policies count too, or a linked key reads as
	// unreachable.
	keyReferenced := map[string]bool{}
	for _, pol := range g.Policies {
		switch pol.Metadata.Owner.Kind {
		case "user", "system", "team", "project":
		default:
			continue
		}
		for _, hk := range pol.Spec.Credentials {
			keyReferenced[hk] = true
		}
		for _, name := range pol.Spec.Pools {
			if p, ok := g.Pools[name]; ok {
				for _, hk := range p.Spec.Credentials {
					keyReferenced[hk] = true
				}
			}
		}
	}
	for name := range g.HostKeys {
		if !keyReferenced[name] {
			out = append(out, Issue{
				Severity: SeverityWarning,
				Kind:     KindOrphan,
				Source:   Ref{Kind: "HostKey", Name: name},
				Message:  "hostkey not linked by any routable policy or its pools; reachable only through its owner's scope",
			})
		}
	}

	// RateLimit → at least one Policy reference.
	rlReferenced := map[string]bool{}
	for _, pol := range g.Policies {
		for _, l := range pol.Spec.RateLimits {
			if l.Ref != "" {
				rlReferenced[l.Ref] = true
			}
		}
	}
	for name := range g.RateLimits {
		if !rlReferenced[name] {
			out = append(out, Issue{
				Severity: SeverityWarning,
				Kind:     KindOrphan,
				Source:   Ref{Kind: "RateLimit", Name: name},
				Message:  "rate limit not referenced by any policy",
			})
		}
	}

	return out
}
