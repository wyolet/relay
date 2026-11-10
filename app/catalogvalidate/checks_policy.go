package catalogvalidate

import (
	"fmt"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/modelref"
)

// checkPolicyRefs validates Policy outbound refs:
//   - metadata.owner → the Host exists when owner.kind=host
//   - spec.pools[], spec.credentials[], spec.rateLimits[].ref, spec.budgets[]
//     → each name exists
//   - spec.models[] and spec.rateLimits[].models[] → explicit provider,
//     model and host slugs exist
//   - a host-owned (tier) policy links no pools or credentials, and each of
//     its scoped rate limits names models its host serves
func checkPolicyRefs(g *graph) []Issue {
	var out []Issue
	for _, pol := range g.Policies {
		src := Ref{Kind: "Policy", Name: pol.Metadata.Name}
		out = append(out, checkOwnerProject(g, src.Kind, src.Name, pol.Metadata.Owner)...)

		tierHost := ""
		if pol.Metadata.Owner.Kind == "host" {
			tierHost = ownerName(pol.Metadata.Owner.Name, pol.Metadata.Owner.ID)
			if _, ok := g.Hosts[tierHost]; tierHost != "" && !ok {
				out = append(out, refMissing(src, "metadata.owner", "Host", tierHost))
			}
			out = append(out, checkTierLinks(src, pol)...)
		}

		for i, name := range pol.Spec.Pools {
			if _, ok := g.Pools[name]; !ok {
				out = append(out, refMissing(src, fmt.Sprintf("spec.pools[%d]", i), "Pool", name))
			}
		}
		for i, b := range pol.Spec.Budgets {
			field := fmt.Sprintf("spec.budgets[%d]", i)
			if b.Ref == "" {
				out = append(out, Issue{Severity: SeverityError, Kind: KindIncomplete,
					Source: Ref{Kind: src.Kind, Name: src.Name, Field: field + ".ref"}, Message: "ref is required"})
			} else if _, ok := g.Budgets[b.Ref]; !ok {
				out = append(out, refMissing(src, field+".ref", "Budget", b.Ref))
			}
		}
		for i, l := range pol.Spec.RateLimits {
			field := fmt.Sprintf("spec.rateLimits[%d]", i)
			if l.Ref == "" {
				out = append(out, Issue{Severity: SeverityError, Kind: KindIncomplete,
					Source: Ref{Kind: src.Kind, Name: src.Name, Field: field + ".ref"}, Message: "ref is required"})
			} else if _, ok := g.RateLimits[l.Ref]; !ok {
				out = append(out, refMissing(src, field+".ref", "RateLimit", l.Ref))
			}
			for j, ref := range l.Models {
				refField := fmt.Sprintf("%s.models[%d]", field, j)
				out = append(out, validateModelRef(ref, src, refField, g)...)
				if tierHost != "" {
					out = append(out, checkServedAtHost(g, src, refField, ref, tierHost)...)
				}
			}
		}

		for i, ref := range pol.Spec.Models {
			out = append(out, validateModelRef(ref, src, fmt.Sprintf("spec.models[%d]", i), g)...)
		}
	}
	return out
}

// checkTierLinks refuses pools on a host-owned policy: a tier describes what
// the provider grants an account, not which keys a caller may spend.
func checkTierLinks(src Ref, pol *manifest.PolicyDTO) []Issue {
	if len(pol.Spec.Pools) == 0 {
		return nil
	}
	return []Issue{{Severity: SeverityError, Kind: KindInvariant,
		Source:  Ref{Kind: src.Kind, Name: src.Name, Field: "spec.pools"},
		Message: "a host-owned tier policy links no pools"}}
}

// checkServedAtHost requires a tier policy's scoped model ref to match at
// least one enabled binding at the tier's host; otherwise the limit counts
// nothing and the tier is quietly uncapped for the model it meant.
func checkServedAtHost(g *graph, src Ref, field, raw, host string) []Issue {
	ref, err := modelref.Parse(raw)
	if err != nil {
		return nil
	}
	for _, b := range g.HostBindings {
		if b.Spec.Host != host || (b.Spec.Enabled != nil && !*b.Spec.Enabled) {
			continue
		}
		provider := ""
		if m, ok := g.Models[b.Spec.Model]; ok && m.Metadata.Owner.Kind == "provider" {
			provider = ownerName(m.Metadata.Owner.Name, m.Metadata.Owner.ID)
		}
		if ref.Matches(provider, b.Spec.Model, host) {
			return nil
		}
	}
	return []Issue{{
		Severity: SeverityError,
		Kind:     KindInvariant,
		Source:   Ref{Kind: src.Kind, Name: src.Name, Field: field},
		Target:   Ref{Kind: "Host", Name: host},
		Message:  fmt.Sprintf("modelref %q matches no model host %q serves", raw, host),
	}}
}
