package catalog

import "strings"

// SearchHit is one (model, host) binding that matched a Search.
type SearchHit struct {
	Model       string // Binding.MetadataName
	Host        string
	DisplayName string
}

// Search returns every binding whose model slug, wire name, display name or provider contains text, case-insensitively, in catalog order. Empty text matches everything.
func (ic *IndexedCatalog) Search(text string) []SearchHit {
	needle := strings.ToLower(text)
	infos := make(map[string]ModelInfo, len(ic.Catalog.Models))
	for _, m := range ic.Catalog.Models {
		infos[m.MetadataName] = m
	}
	var hits []SearchHit
	for _, h := range ic.Catalog.Hosts {
		for _, b := range h.Models {
			info := infos[b.MetadataName]
			fields := append([]string{b.MetadataName, b.Name, info.DisplayName, info.Provider}, b.Providers...)
			for _, f := range fields {
				if strings.Contains(strings.ToLower(f), needle) {
					hits = append(hits, SearchHit{Model: b.MetadataName, Host: h.Name, DisplayName: info.DisplayName})
					break
				}
			}
		}
	}
	return hits
}
