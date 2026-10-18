package curation

import (
	"fmt"

	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

// Pricing is a (Host, Model) tuple: pricing a model that has no HostBinding
// to the owning host yields a price row that never applies at request time.
var pricingTargetMustBindToHost = catalogvalidate.Rule{
	Name:        "pricing-target-must-bind-to-host",
	Description: "Pricing.targetModels must each declare a HostBinding to the Pricing's owner host",
	Severity:    catalogvalidate.SeverityError,
	Check:       checkPricingTargetHostBinding,
}

func checkPricingTargetHostBinding(docs []manifest.Document) []catalogvalidate.Issue {
	models := map[string]bool{}
	boundHosts := map[string]map[string]bool{} // model name → host names it binds to
	for _, d := range docs {
		switch {
		case d.Model != nil:
			models[d.Model.Metadata.Name] = true
		case d.HostBinding != nil:
			m, h := d.HostBinding.Spec.Model, d.HostBinding.Spec.Host
			if boundHosts[m] == nil {
				boundHosts[m] = map[string]bool{}
			}
			boundHosts[m][h] = true
		}
	}

	var out []catalogvalidate.Issue
	for _, d := range docs {
		if d.Pricing == nil {
			continue
		}
		host := d.Pricing.Metadata.Owner.Name
		if host == "" {
			host = d.Pricing.Metadata.Owner.ID
		}
		// A missing or non-host owner is reported by ValidateGraph and the
		// entity's own Validate.
		if d.Pricing.Metadata.Owner.Kind != "host" || host == "" {
			continue
		}
		for i, mname := range d.Pricing.Spec.TargetModels {
			if !models[mname] {
				continue // ValidateGraph reports the missing ref
			}
			if !boundHosts[mname][host] {
				out = append(out, catalogvalidate.Issue{
					Severity: catalogvalidate.SeverityError,
					Kind:     catalogvalidate.KindInvariant,
					Source: catalogvalidate.Ref{
						Kind:  "Pricing",
						Name:  d.Pricing.Metadata.Name,
						Field: fmt.Sprintf("spec.targetModels[%d]", i),
					},
					Target:  catalogvalidate.Ref{Kind: "Model", Name: mname},
					Message: fmt.Sprintf("model %q has no host binding to %q — pricing row will never apply", mname, host),
				})
			}
		}
	}
	return out
}
