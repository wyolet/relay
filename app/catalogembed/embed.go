// Package catalogembed composes manifest YAML into the SDK catalog embed schema.
// Server-module only — imports app/* and writes sdk/catalog/catalog.json.
package catalogembed

import (
	"encoding/json"
	"fmt"
	"sort"

	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// SDKAdapters is the adapter-name set the public SDK client supports today.
var SDKAdapters = map[string]struct{}{
	"openai":           {},
	"openai_responses": {},
	"anthropic":        {},
	"gemini":           {},
}

// ValidateAdapters returns an error if any binding uses an adapter name outside
// SDKAdapters.
func ValidateAdapters(c *sdkcatalog.Catalog) error {
	for _, h := range c.Hosts {
		for _, b := range h.Models {
			if _, ok := SDKAdapters[b.Adapter]; !ok {
				return fmt.Errorf("catalog-embed: host %q model %q: unknown adapter %q", h.Name, b.MetadataName, b.Adapter)
			}
		}
	}
	return nil
}

// MarshalJSON encodes c deterministically (sorted hosts and bindings).
func MarshalJSON(c *sdkcatalog.Catalog) ([]byte, error) {
	cp := *c
	cp.Hosts = append([]sdkcatalog.Host(nil), c.Hosts...)
	sort.Slice(cp.Hosts, func(i, j int) bool { return cp.Hosts[i].Name < cp.Hosts[j].Name })
	for i := range cp.Hosts {
		models := append([]sdkcatalog.Binding(nil), cp.Hosts[i].Models...)
		sort.Slice(models, func(a, b int) bool {
			if models[a].MetadataName != models[b].MetadataName {
				return models[a].MetadataName < models[b].MetadataName
			}
			return models[a].Adapter < models[b].Adapter
		})
		cp.Hosts[i].Models = models
	}
	cp.Models = append([]sdkcatalog.ModelInfo(nil), c.Models...)
	sort.Slice(cp.Models, func(a, b int) bool { return cp.Models[a].MetadataName < cp.Models[b].MetadataName })
	cp.Providers = append([]sdkcatalog.ProviderInfo(nil), c.Providers...)
	sort.Slice(cp.Providers, func(a, b int) bool { return cp.Providers[a].Name < cp.Providers[b].Name })
	return json.MarshalIndent(&cp, "", "  ")
}
