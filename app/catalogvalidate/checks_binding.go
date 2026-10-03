package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/adapters"
)

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
//   - spec.adapter → must be a dispatchable upstream binding when set
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

		// spec.adapter — empty takes the default; anything else must be dispatchable.
		if b.Spec.Adapter != "" && !adapters.Name(b.Spec.Adapter).UpstreamBinding() {
			out = append(out, Issue{
				Severity: SeverityError,
				Kind:     KindInvariant,
				Source:   Ref{Kind: src.Kind, Name: src.Name, Field: "spec.adapter"},
				Message:  fmt.Sprintf("adapter %q is not a valid upstream binding (want one of %v)", b.Spec.Adapter, adapters.UpstreamBindingNames()),
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
