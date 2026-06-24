// Package model is the public, read-only discovery view of a catalogued model:
// its identity, capabilities, the provider that authored it, and the hosts that
// serve it. Resolve a model ref (bare slug, provider/model, wire name, or a
// declared alias) and navigate to its author and hosts; everything is plain,
// JSON-serializable data for rendering a catalog UI.
//
// This is not the call path — constructing a client and making requests lives in
// sdk/client and is unaffected by anything here.
package model

import "github.com/wyolet/relay/sdk/internal/graph"

// Model and the types reachable from it. These are aliases to the internal
// discovery graph, so a *Model navigates to *host.Host / *provider.Provider with
// no conversions.
type (
	Model         = graph.Model
	ModelHost     = graph.ModelHost
	Capabilities  = graph.Capabilities
	Modalities    = graph.Modalities
	ContextWindow = graph.ContextWindow
	Rate          = graph.Rate
	Host          = graph.Host
	Provider      = graph.Provider
)

// Resolve returns the model for a ref, aggregating every host that serves it.
// Ref forms: "gpt-4o", "openai/gpt-4o", the provider wire name, or a declared
// alias. Unlike a call-path resolve, a model served on several hosts is one
// Model with several Hosts — not an ambiguity error.
func Resolve(ref string) (*Model, error) { return graph.ResolveModel(ref) }
