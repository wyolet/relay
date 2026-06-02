package catalogvalidate

import "fmt"

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

// checkHostRefs validates Host outbound refs:
//   - spec.defaultPolicy → Policy name (must exist if non-empty)
func checkHostRefs(g *graph) []Issue {
	var out []Issue
	for _, h := range g.Hosts {
		if h.Spec.DefaultPolicy == "" {
			continue
		}
		if _, ok := g.Policies[h.Spec.DefaultPolicy]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: "Host", Name: h.Metadata.Name, Field: "spec.defaultPolicy"},
				Target:   Ref{Kind: "Policy", Name: h.Spec.DefaultPolicy},
				Message:  fmt.Sprintf("defaultPolicy %q not found", h.Spec.DefaultPolicy),
			})
		}
	}
	return out
}

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

// checkHostKeyRefs validates HostKey outbound refs and the host/tier-policy
// invariant:
//   - spec.hostId → Host name must exist
//   - spec.policyId → Policy name must exist, and policy.metadata.owner
//     must be the same host (host-owned tier policies)
func checkHostKeyRefs(g *graph) []Issue {
	var out []Issue
	for _, hk := range g.HostKeys {
		src := Ref{Kind: "HostKey", Name: hk.Metadata.Name}

		if hk.Spec.HostID == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.hostId"},
				Message:  "hostId is required",
			})
		} else if _, ok := g.Hosts[hk.Spec.HostID]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.hostId"},
				Target:   Ref{Kind: "Host", Name: hk.Spec.HostID},
				Message:  fmt.Sprintf("host %q not found", hk.Spec.HostID),
			})
		}

		if hk.Spec.PolicyID == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policyId"},
				Message:  "policyId is required (must name a host-owned tier policy)",
			})
			continue
		}
		pol, ok := g.Policies[hk.Spec.PolicyID]
		if !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policyId"},
				Target:   Ref{Kind: "Policy", Name: hk.Spec.PolicyID},
				Message:  fmt.Sprintf("policy %q not found", hk.Spec.PolicyID),
			})
			continue
		}
		// Host/tier-policy ownership: hostkey.hostId must equal the
		// host-owned policy's owner.name.
		if pol.Metadata.Owner.Kind != "host" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindOwnerMismatch,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policyId"},
				Target:   Ref{Kind: "Policy", Name: hk.Spec.PolicyID},
				Message:  fmt.Sprintf("hostkey references policy %q which is not host-owned (owner.kind=%q)", hk.Spec.PolicyID, pol.Metadata.Owner.Kind),
			})
			continue
		}
		polHost := pol.Metadata.Owner.Name
		if polHost == "" {
			polHost = pol.Metadata.Owner.ID
		}
		if polHost != hk.Spec.HostID {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindOwnerMismatch,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.policyId"},
				Target:   Ref{Kind: "Policy", Name: hk.Spec.PolicyID},
				Message:  fmt.Sprintf("hostkey on host %q references policy owned by host %q", hk.Spec.HostID, polHost),
			})
		}
	}
	return out
}

// checkPolicyRefs validates Policy outbound refs:
//   - spec.hostKeys[] → HostKey names must exist
//   - spec.rateLimit → RateLimit name must exist when set
//   - spec.rlBindings[].rateLimit → RateLimit names must exist
//   - spec.models[] are modelref DSL strings; best-effort parse + check
//     that referenced provider/host slugs (if explicit) exist
//   - metadata.owner.id/name → resolves to Host when owner.kind=host
func checkPolicyRefs(g *graph) []Issue {
	var out []Issue
	for _, pol := range g.Policies {
		src := Ref{Kind: "Policy", Name: pol.Metadata.Name}

		if pol.Metadata.Owner.Kind == "host" {
			hname := pol.Metadata.Owner.Name
			if hname == "" {
				hname = pol.Metadata.Owner.ID
			}
			if hname != "" {
				if _, ok := g.Hosts[hname]; !ok {
					out = append(out, Issue{
						Severity: SeverityError,
						Kind:     KindRefMissing,
						Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "metadata.owner"},
						Target:   Ref{Kind: "Host", Name: hname},
						Message:  fmt.Sprintf("owner host %q not found", hname),
					})
				}
			}
		}

		for i, hkName := range pol.Spec.HostKeys {
			if _, ok := g.HostKeys[hkName]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source: Ref{
						Kind:  src.Kind,
						Name:  src.Name,
						Field: fmt.Sprintf("spec.hostKeys[%d]", i),
					},
					Target:  Ref{Kind: "HostKey", Name: hkName},
					Message: fmt.Sprintf("hostKey %q not found", hkName),
				})
			}
		}

		if pol.Spec.RateLimit != "" {
			if _, ok := g.RateLimits[pol.Spec.RateLimit]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.rateLimit"},
					Target:   Ref{Kind: "RateLimit", Name: pol.Spec.RateLimit},
					Message:  fmt.Sprintf("rateLimit %q not found", pol.Spec.RateLimit),
				})
			}
		}

		for i, b := range pol.Spec.RLBindings {
			if b.RateLimit == "" {
				continue
			}
			if _, ok := g.RateLimits[b.RateLimit]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source: Ref{
						Kind:  src.Kind,
						Name:  src.Name,
						Field: fmt.Sprintf("spec.rlBindings[%d].rateLimit", i),
					},
					Target:  Ref{Kind: "RateLimit", Name: b.RateLimit},
					Message: fmt.Sprintf("rateLimit %q not found", b.RateLimit),
				})
			}
		}

		// modelref DSL strings in spec.models — best-effort parse.
		// Format: "<provider-slug>", "<provider>/<model>", "@<host>",
		// "<provider>/<model>@<host>", "<provider>/*", etc.
		for i, ref := range pol.Spec.Models {
			issues := validateModelRef(ref, pol.Metadata.Name, i, g)
			out = append(out, issues...)
		}
	}
	return out
}

// checkPricingRefs validates Pricing outbound refs:
//   - metadata.owner (kind=host) → Host name must exist
//   - spec.targetModels[] → Model names must exist
func checkPricingRefs(g *graph) []Issue {
	var out []Issue
	for _, pr := range g.Pricings {
		src := Ref{Kind: "Pricing", Name: pr.Metadata.Name}

		if pr.Metadata.Owner.Kind == "host" {
			hname := pr.Metadata.Owner.Name
			if hname == "" {
				hname = pr.Metadata.Owner.ID
			}
			if hname != "" {
				if _, ok := g.Hosts[hname]; !ok {
					out = append(out, Issue{
						Severity: SeverityError,
						Kind:     KindRefMissing,
						Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "metadata.owner"},
						Target:   Ref{Kind: "Host", Name: hname},
						Message:  fmt.Sprintf("owner host %q not found", hname),
					})
				}
			}
		}

		for i, mname := range pr.Spec.TargetModels {
			if _, ok := g.Models[mname]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source: Ref{
						Kind:  src.Kind,
						Name:  src.Name,
						Field: fmt.Sprintf("spec.targetModels[%d]", i),
					},
					Target:  Ref{Kind: "Model", Name: mname},
					Message: fmt.Sprintf("targetModel %q not found", mname),
				})
			}
		}
	}
	return out
}

// checkBindingRefs validates HostBinding outbound refs:
//   - spec.model → Model name must exist
//   - spec.host → Host name must exist
//   - spec.pricing → Pricing name must exist when set
//   - spec.snapshots[] → must be a subset of the referenced model's snapshot names
//   - no duplicate (model, host) pairs
func checkBindingRefs(g *graph) []Issue {
	var out []Issue
	// Track (model, host) pairs to detect duplicates.
	seen := map[[2]string]string{} // value = first binding name that claimed the pair

	for _, b := range g.HostBindings {
		src := Ref{Kind: "HostBinding", Name: b.Metadata.Name}

		// spec.model
		if b.Spec.Model == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.model"},
				Message:  "model is required",
			})
		} else if _, ok := g.Models[b.Spec.Model]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.model"},
				Target:   Ref{Kind: "Model", Name: b.Spec.Model},
				Message:  fmt.Sprintf("model %q not found", b.Spec.Model),
			})
		}

		// spec.host
		if b.Spec.Host == "" {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindIncomplete,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.host"},
				Message:  "host is required",
			})
		} else if _, ok := g.Hosts[b.Spec.Host]; !ok {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindRefMissing,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.host"},
				Target:   Ref{Kind: "Host", Name: b.Spec.Host},
				Message:  fmt.Sprintf("host %q not found", b.Spec.Host),
			})
		}

		// spec.pricing (optional)
		if b.Spec.Pricing != "" {
			if _, ok := g.Pricings[b.Spec.Pricing]; !ok {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindRefMissing,
					Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.pricing"},
					Target:   Ref{Kind: "Pricing", Name: b.Spec.Pricing},
					Message:  fmt.Sprintf("pricing %q not found", b.Spec.Pricing),
				})
			}
		}

		// spec.snapshots — must be subset of model's snapshots
		if b.Spec.Model != "" {
			if m, ok := g.Models[b.Spec.Model]; ok {
				snapNames := make(map[string]struct{}, len(m.Spec.Snapshots))
				for _, s := range m.Spec.Snapshots {
					snapNames[s.Name] = struct{}{}
				}
				for si, sn := range b.Spec.Snapshots {
					if _, ok := snapNames[sn]; !ok {
						out = append(out, Issue{
							Severity: SeverityError,
							Kind:     KindSnapshotMissing,
							Source: Ref{
								Kind:  src.Kind,
								Name:  src.Name,
								Field: fmt.Sprintf("spec.snapshots[%d]", si),
							},
							Message: fmt.Sprintf("snapshot %q not declared in model %q spec.snapshots", sn, b.Spec.Model),
						})
					}
				}
			}
		}

		// Duplicate (model, host) pair check.
		if b.Spec.Model != "" && b.Spec.Host != "" {
			pair := [2]string{b.Spec.Model, b.Spec.Host}
			if first, dup := seen[pair]; dup {
				out = append(out, Issue{
					Severity: SeverityError,
					Kind:     KindInvariant,
					Source:   src,
					Message:  fmt.Sprintf("duplicate (model=%q, host=%q) binding; first declared by %q", b.Spec.Model, b.Spec.Host, first),
				})
			} else {
				seen[pair] = b.Metadata.Name
			}
		}
	}
	return out
}

// checkRelayKeyRefs validates RelayKey outbound refs:
//   - spec.policy → Policy name must exist
func checkRelayKeyRefs(g *graph) []Issue {
	var out []Issue
	for _, rk := range g.RelayKeys {
		src := Ref{Kind: "RelayKey", Name: rk.Metadata.Name}
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

// checkOrphans surfaces curation hints (warnings, not errors):
//   - Provider with zero Models
//   - Model with zero enabled host bindings
//   - HostKey not referenced by any user-owned Policy (unreachable)
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

	// HostKey → at least one user-owned Policy listing it.
	keyReferenced := map[string]bool{}
	for _, pol := range g.Policies {
		if pol.Metadata.Owner.Kind != "user" && pol.Metadata.Owner.Kind != "system" {
			continue
		}
		for _, hk := range pol.Spec.HostKeys {
			keyReferenced[hk] = true
		}
	}
	for name := range g.HostKeys {
		if !keyReferenced[name] {
			out = append(out, Issue{
				Severity: SeverityWarning,
				Kind:     KindOrphan,
				Source:   Ref{Kind: "HostKey", Name: name},
				Message:  "hostkey not referenced by any user/system-owned policy; underlying models won't appear in /v1/models",
			})
		}
	}

	// RateLimit → at least one Policy reference.
	rlReferenced := map[string]bool{}
	for _, pol := range g.Policies {
		if pol.Spec.RateLimit != "" {
			rlReferenced[pol.Spec.RateLimit] = true
		}
		for _, b := range pol.Spec.RLBindings {
			if b.RateLimit != "" {
				rlReferenced[b.RateLimit] = true
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
