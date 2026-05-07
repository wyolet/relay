package provider

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/transport"
)

// Outbound is implemented by every upstream provider client.
// Concrete implementations live in subpackages (ollama, openai, ...).
//
// secret is the resolved secret value (e.g., the OpenAI API key).
// Empty string is acceptable for anonymous-auth providers.
//
// The Outbound emits response chunks as *transport.Messages on out
// and is responsible for closing out before returning. The first
// Message must carry Headers["X-Relay-Status"] and Headers["Content-Type"];
// subsequent Messages carry body chunks; the final Message carries
// Headers["X-Relay-Final"] = "true".
type Outbound interface {
	ChatCompletions(ctx context.Context, body []byte, secret string, out chan<- *transport.Message) error
}

// Registry maps a catalog.ProviderKind to an Outbound implementation.
type Registry struct {
	byKind map[catalog.ProviderKind]Outbound
}

func NewRegistry() *Registry {
	return &Registry{byKind: make(map[catalog.ProviderKind]Outbound)}
}

// Register associates a kind with an Outbound. Last write wins.
func (r *Registry) Register(kind catalog.ProviderKind, o Outbound) {
	r.byKind[kind] = o
}

// Get returns the Outbound for a kind, or an error if none registered.
func (r *Registry) Get(kind catalog.ProviderKind) (Outbound, error) {
	o, ok := r.byKind[kind]
	if !ok {
		return nil, fmt.Errorf("provider: no outbound registered for kind %q", kind)
	}
	return o, nil
}
