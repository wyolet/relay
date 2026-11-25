package pseudo

import "github.com/wyolet/relay/pkg/slug"

// Namer mints row names as the control API's creates do: a slug of a base, suffixed -2, -3, … while a stored row of the kind holds it or an earlier row of the same expansion took it.
type Namer struct {
	// Taken reports whether a stored row of the API kind holds name.
	Taken  func(kind, name string) bool
	minted map[string]map[string]bool
}

// Mint returns a free name of kind from base, or from fallback when base slugs to nothing, and holds it for the rest of the expansion.
func (n *Namer) Mint(kind, base, fallback string) string {
	b := slug.From(base)
	if b == "" {
		b = fallback
	}
	name := slug.Unique(b, func(c string) bool { return n.minted[kind][c] || n.Taken(kind, c) })
	n.Hold(kind, name)
	return name
}

// Hold keeps name of kind from the rows minted after it: a name the body gave.
func (n *Namer) Hold(kind, name string) {
	if n.minted == nil {
		n.minted = map[string]map[string]bool{}
	}
	if n.minted[kind] == nil {
		n.minted[kind] = map[string]bool{}
	}
	n.minted[kind][name] = true
}
