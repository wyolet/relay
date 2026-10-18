package curation

import (
	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

// A host without a tier Policy gives its hostkeys no rate-limit ladder to
// anchor on. Some hosts have no formal tiers on purpose, hence a warning.
var hostHasTierPolicy = catalogvalidate.Rule{
	Name:        "host-has-tier-policy",
	Description: "Host should declare at least one Policy owned by it (its tier ladder)",
	Severity:    catalogvalidate.SeverityWarning,
	Check:       checkHostHasTierPolicy,
}

func checkHostHasTierPolicy(docs []manifest.Document) []catalogvalidate.Issue {
	tierCount := map[string]int{}
	for _, d := range docs {
		if d.Policy == nil {
			continue
		}
		if d.Policy.Metadata.Owner.Kind != "host" {
			continue
		}
		hname := d.Policy.Metadata.Owner.Name
		if hname == "" {
			hname = d.Policy.Metadata.Owner.ID
		}
		if hname != "" {
			tierCount[hname]++
		}
	}

	var out []catalogvalidate.Issue
	for _, d := range docs {
		if d.Host == nil {
			continue
		}
		name := d.Host.Metadata.Name
		if tierCount[name] == 0 {
			out = append(out, catalogvalidate.Issue{
				Severity: catalogvalidate.SeverityWarning,
				Kind:     catalogvalidate.KindIncomplete,
				Source:   catalogvalidate.Ref{Kind: "Host", Name: name},
				Message:  "host has no tier policies — hostkeys can't anchor a rate-limit ladder",
			})
		}
	}
	return out
}
