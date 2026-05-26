package secret

import (
	"context"
	"fmt"
)

// Registry dispatches a Ref to the Resolver registered for its Kind. Built
// once at boot (Register is not safe for concurrent use with Resolve);
// Resolve is safe for concurrent use thereafter.
type Registry struct {
	byKind map[Kind]Resolver
}

// NewRegistry returns an empty registry. Register backends before use.
func NewRegistry() *Registry {
	return &Registry{byKind: map[Kind]Resolver{}}
}

// Register binds a Resolver to a Kind, replacing any prior registration.
func (r *Registry) Register(kind Kind, res Resolver) {
	r.byKind[kind] = res
}

// Resolve validates the Ref and dispatches to the registered Resolver.
// Returns a clear error when no backend is registered for the Kind — e.g.
// a "stored" ref in a minimal build that wired only env.
func (r *Registry) Resolve(ctx context.Context, ref Ref) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	res, ok := r.byKind[ref.Kind]
	if !ok {
		return nil, fmt.Errorf("secret: no resolver registered for kind %q", ref.Kind)
	}
	return res.Resolve(ctx, ref)
}
