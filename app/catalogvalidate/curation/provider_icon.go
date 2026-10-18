package curation

import (
	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

var providerIconSet = catalogvalidate.Rule{
	Name:        "provider-icon-set",
	Description: "Provider should declare spec.icon.path so the UI can render its avatar",
	Severity:    catalogvalidate.SeverityWarning,
	Check:       checkProviderIcon,
}

func checkProviderIcon(docs []manifest.Document) []catalogvalidate.Issue {
	var out []catalogvalidate.Issue
	for _, d := range docs {
		if d.Provider == nil {
			continue
		}
		if d.Provider.Spec.Icon == nil || d.Provider.Spec.Icon.Path == "" {
			out = append(out, catalogvalidate.Issue{
				Severity: catalogvalidate.SeverityWarning,
				Kind:     catalogvalidate.KindIncomplete,
				Source:   catalogvalidate.Ref{Kind: "Provider", Name: d.Provider.Metadata.Name, Field: "spec.icon.path"},
				Message:  "provider has no icon — UI will render a placeholder",
			})
		}
	}
	return out
}
