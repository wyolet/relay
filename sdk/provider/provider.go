// Package provider is the public, read-only discovery view of a model author
// (the vendor): its display metadata and the slugs of the models it authors.
// Pair it with sdk/model to navigate the catalog. This is not the call path.
package provider

import "github.com/wyolet/relay/sdk/internal/graph"

// Provider is the discovery node for a model author (alias to the internal graph
// type, shared with model.Model.Author).
type Provider = graph.Provider

// Resolve returns the provider with the given catalog name.
func Resolve(ref string) (*Provider, error) { return graph.ResolveProvider(ref) }
