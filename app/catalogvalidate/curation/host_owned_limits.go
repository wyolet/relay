package curation

import (
	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

// A catalog's tiers and their rate limits belong to their host. A deployment's system policies and rate limits are its own: the boot that moves them to the organization relies on the catalog shipping none, and a seed would otherwise take an operator's row of that name. An owner left out seeds as system, so it is refused too.
var hostOwnedLimits = catalogvalidate.Rule{
	Name:        "host-owned-limits",
	Description: "Policies and RateLimits are owned by a host, never the system",
	Severity:    catalogvalidate.SeverityError,
	Check:       checkHostOwnedLimits,
}

func checkHostOwnedLimits(docs []manifest.Document) []catalogvalidate.Issue {
	var out []catalogvalidate.Issue
	refuse := func(kind, name string, owner manifest.WireOwner) {
		if owner.Kind != "" && owner.Kind != "system" {
			return
		}
		out = append(out, catalogvalidate.Issue{
			Severity: catalogvalidate.SeverityError,
			Kind:     catalogvalidate.KindInvariant,
			Source:   catalogvalidate.Ref{Kind: kind, Name: name, Field: "metadata.owner"},
			Message:  "a catalog " + kind + " must be owned by its host, not the system",
		})
	}
	for _, d := range docs {
		switch {
		case d.Policy != nil:
			refuse("Policy", d.Policy.Metadata.Name, d.Policy.Metadata.Owner)
		case d.RateLimit != nil:
			refuse("RateLimit", d.RateLimit.Metadata.Name, d.RateLimit.Metadata.Owner)
		}
	}
	return out
}
