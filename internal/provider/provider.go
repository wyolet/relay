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

// registryKey uniquely identifies a provider client by kind and base URL.
// Caching by (Kind, BaseURL) means that a baseURL change in the catalog
// automatically produces a cache miss and causes outboundFor to create a
// fresh client — no explicit invalidation needed on admin writes.
type registryKey struct {
	Kind    catalog.ProviderKind
	BaseURL string
}

// Registry maps a (catalog.ProviderKind, baseURL) pair to an Outbound implementation.
type Registry struct {
	byKey map[registryKey]Outbound
}

func NewRegistry() *Registry {
	return &Registry{byKey: make(map[registryKey]Outbound)}
}

// Register associates a (kind, baseURL) pair with an Outbound. Last write wins.
func (r *Registry) Register(kind catalog.ProviderKind, baseURL string, o Outbound) {
	r.byKey[registryKey{Kind: kind, BaseURL: baseURL}] = o
}

// Get returns the Outbound for a (kind, baseURL) pair, or an error if none registered.
func (r *Registry) Get(kind catalog.ProviderKind, baseURL string) (Outbound, error) {
	o, ok := r.byKey[registryKey{Kind: kind, BaseURL: baseURL}]
	if !ok {
		return nil, fmt.Errorf("provider: no outbound registered for kind %q baseURL %q", kind, baseURL)
	}
	return o, nil
}
