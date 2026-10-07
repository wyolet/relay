package modeldir

import "github.com/wyolet/relay/sdk/catalog"

// Model is one model file. Keys mirror the catalog's json names so a file converts to and from catalog.Binding and catalog.ModelInfo by copying fields.
type Model struct {
	Name            string               `yaml:"name"`
	WireName        string               `yaml:"wireName,omitempty"`
	Provider        string               `yaml:"provider,omitempty"`
	Adapter         string               `yaml:"adapter,omitempty"`
	DisplayName     string               `yaml:"displayName,omitempty"`
	Aliases         []string             `yaml:"aliases,omitempty"`
	ContextWindow   ContextWindow        `yaml:"contextWindow,omitempty"`
	MaxOutputTokens int                  `yaml:"maxOutputTokens,omitempty"`
	Capabilities    catalog.Capabilities `yaml:"capabilities,omitempty"`
	Modalities      catalog.Modalities   `yaml:"modalities,omitempty"`
	Pricing         []catalog.Rate       `yaml:"pricing,omitempty"`
	PricedBy        string               `yaml:"pricedBy,omitempty"`
	Source          *Origin              `yaml:"source,omitempty"`
}

// ContextWindow is the model's token limits.
type ContextWindow struct {
	Input  int `yaml:"input,omitempty"`
	Output int `yaml:"output,omitempty"`
	Total  int `yaml:"total,omitempty"`
}

// Origin records where a file was derived from. A file without one is hand-written and Refresh leaves it alone.
type Origin struct {
	Catalog string `yaml:"catalog"`
}

// modelFromCatalog copies one binding and its model metadata into a file.
func modelFromCatalog(b catalog.Binding, h catalog.Host, info catalog.ModelInfo, version string) Model {
	provider := info.Provider
	if provider == "" && len(b.Providers) > 0 {
		provider = b.Providers[0]
	}
	return Model{
		Name:        b.MetadataName,
		WireName:    b.Name,
		Provider:    provider,
		Adapter:     b.Adapter,
		DisplayName: info.DisplayName,
		Aliases:     b.Aliases,
		ContextWindow: ContextWindow{
			Input:  info.ContextWindowInput,
			Output: info.ContextWindowOutput,
			Total:  info.ContextWindowTotal,
		},
		MaxOutputTokens: info.MaxOutputTokens,
		Capabilities:    info.Capabilities,
		Modalities:      info.Modalities,
		Pricing:         b.Pricing,
		PricedBy:        h.Name,
		Source:          &Origin{Catalog: version},
	}
}

// buildCatalog assembles the catalog Load indexes: one host per distinct pricedBy, one binding and one ModelInfo per file.
func buildCatalog(models []Model) *catalog.Catalog {
	c := &catalog.Catalog{}
	hostIndex := map[string]int{}
	for _, m := range models {
		hi, ok := hostIndex[m.PricedBy]
		if !ok {
			hi = len(c.Hosts)
			hostIndex[m.PricedBy] = hi
			c.Hosts = append(c.Hosts, catalog.Host{Name: m.PricedBy})
		}
		c.Hosts[hi].Models = append(c.Hosts[hi].Models, m.binding())
		c.Models = append(c.Models, m.info())
	}
	return c
}

func (m Model) binding() catalog.Binding {
	wire := m.WireName
	if wire == "" {
		wire = m.Name
	}
	var providers []string
	if m.Provider != "" {
		providers = []string{m.Provider}
	}
	return catalog.Binding{
		Name:         wire,
		MetadataName: m.Name,
		Adapter:      m.Adapter,
		Providers:    providers,
		Pricing:      m.Pricing,
		Aliases:      m.Aliases,
	}
}

func (m Model) info() catalog.ModelInfo {
	return catalog.ModelInfo{
		MetadataName:        m.Name,
		Provider:            m.Provider,
		DisplayName:         m.DisplayName,
		Capabilities:        m.Capabilities,
		Modalities:          m.Modalities,
		ContextWindowInput:  m.ContextWindow.Input,
		ContextWindowOutput: m.ContextWindow.Output,
		ContextWindowTotal:  m.ContextWindow.Total,
		MaxOutputTokens:     m.MaxOutputTokens,
	}
}
