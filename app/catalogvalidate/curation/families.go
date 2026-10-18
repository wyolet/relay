package curation

import (
	"fmt"

	"github.com/wyolet/relay/app/catalogvalidate"
	"github.com/wyolet/relay/app/manifest"
)

// CheckFamilies reports every model whose spec.family is missing or not a
// family in lines (families.File.LineNames). A family is the base model
// lineage, so a versioned or product-line value fails here instead of
// splitting a lineage into many families.
func CheckFamilies(docs []manifest.Document, lines map[string]map[string]bool) []catalogvalidate.Issue {
	var out []catalogvalidate.Issue
	for _, d := range docs {
		if d.Model == nil {
			continue
		}
		if _, ok := lines[d.Model.Spec.Family]; ok {
			continue
		}
		msg := fmt.Sprintf("%q is not in families.yaml — use the base model's family, or add a new lineage there", d.Model.Spec.Family)
		if d.Model.Spec.Family == "" {
			msg = "missing — every model names its base model lineage from families.yaml"
		}
		out = append(out, catalogvalidate.Issue{
			Severity: catalogvalidate.SeverityError,
			Kind:     catalogvalidate.KindInvariant,
			Source:   catalogvalidate.Ref{Kind: "Model", Name: d.Model.Metadata.Name, Field: "spec.family"},
			Message:  msg,
		})
	}
	return out
}

// CheckProductLines reports every model whose spec.productLine is set but
// not a line of its family. Empty is always allowed; an unknown family is
// left to CheckFamilies.
func CheckProductLines(docs []manifest.Document, lines map[string]map[string]bool) []catalogvalidate.Issue {
	var out []catalogvalidate.Issue
	for _, d := range docs {
		if d.Model == nil || d.Model.Spec.ProductLine == "" {
			continue
		}
		famLines, ok := lines[d.Model.Spec.Family]
		if !ok || famLines[d.Model.Spec.ProductLine] {
			continue
		}
		out = append(out, catalogvalidate.Issue{
			Severity: catalogvalidate.SeverityError,
			Kind:     catalogvalidate.KindInvariant,
			Source:   catalogvalidate.Ref{Kind: "Model", Name: d.Model.Metadata.Name, Field: "spec.productLine"},
			Message:  fmt.Sprintf("%q is not a line of family %q in families.yaml — leave it empty, or add the line there", d.Model.Spec.ProductLine, d.Model.Spec.Family),
		})
	}
	return out
}
