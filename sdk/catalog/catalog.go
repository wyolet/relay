// Package catalog is the pure SDK view of the public relay catalog: embedded
// host/bindings/pricing data plus model-ref resolution and per-binding cost.
package catalog

import "time"

// Catalog is the embedded catalog payload written by cmd/catalog-embed.
type Catalog struct {
	Version     string    `json:"version"`
	GeneratedAt time.Time `json:"generatedAt"`
	Hosts       []Host    `json:"hosts"`
}

// Host is one upstream serving endpoint and its model bindings.
type Host struct {
	Name    string    `json:"name"`
	BaseURL string    `json:"baseURL"`
	Models  []Binding `json:"models"`
}

// Binding is one callable (snapshot, host) pair with wire metadata. Featured
// carries the catalog's `labels.featured` curation flag (the same one the
// control API exposes) so SDK consumers can shortlist without re-curating.
//
// Name is the model's real provider wire name: the id the provider answers to
// and echoes back as the ran model — the identity a consumer recognizes.
// MetadataName is the catalog key, the DNS-1123 snapshot slug (== catalog
// metadata.name), an internal addressing handle (e.g. name gpt-5.5-2026-04-23,
// MetadataName gpt-5-5-2026-04-23). Both are first-class matchable keys —
// consumers matching a response's model against the catalog must compare it to
// Name AND MetadataName (or just call Resolve, which indexes both). The json
// tags are unchanged from the on-wire catalog (`upstream`/`model`).
type Binding struct {
	Name         string   `json:"upstream"`
	MetadataName string   `json:"model"`
	Adapter      string   `json:"adapter"`
	Providers    []string `json:"providers"`
	Featured     bool     `json:"featured,omitempty"`
	Pricing      []Rate   `json:"pricing,omitempty"`

	// Aliases are the model's declared resolution-only matchers, attached
	// to the pointer-snapshot row: exact strings or single-'*' wildcard
	// patterns. Last-priority lookup keys for Resolve, never identity —
	// the relay routes them to this binding and passes the requested
	// string upstream verbatim.
	Aliases []string `json:"aliases,omitempty"`
}

// Rate is one priced meter on a binding.
type Rate struct {
	Meter       string  `json:"meter"`
	Unit        string  `json:"unit"`
	Amount      float64 `json:"amount"`
	AboveTokens int     `json:"aboveTokens,omitempty"`
}
