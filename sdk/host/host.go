// Package host is the public, read-only discovery view of a serving endpoint:
// its base URL, display metadata, and the slugs of the models it serves. Pair it
// with sdk/model to navigate the catalog. This is not the call path.
package host

import "github.com/wyolet/relay/sdk/internal/graph"

// Host is the discovery node for a serving endpoint (alias to the internal graph
// type, shared with model.ModelHost.Host).
type Host = graph.Host

// Resolve returns the host with the given catalog name.
func Resolve(ref string) (*Host, error) { return graph.ResolveHost(ref) }
