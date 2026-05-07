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

// MessagesOutbound is implemented by provider clients that support the
// Anthropic Messages API shape (/v1/messages). It is kept separate from
// Outbound so that the OpenAI path is entirely unaffected.
type MessagesOutbound interface {
	Messages(ctx context.Context, body []byte, secret string, out chan<- *transport.Message) error
}

// registryKey uniquely identifies a provider client by kind and base URL.
// Caching by (Kind, BaseURL) means that a baseURL change in the catalog
// automatically produces a cache miss and causes outboundFor to create a
// fresh client — no explicit invalidation needed on admin writes.
type registryKey struct {
	Kind    catalog.ProviderKind
	BaseURL string
}

// Registry maps a (catalog.ProviderKind, baseURL) pair to an Outbound or
// MessagesOutbound implementation. Values are stored as `any` so that
// both interface types can coexist in the same map; callers type-assert.
type Registry struct {
	byKey map[registryKey]any
}

func NewRegistry() *Registry {
	return &Registry{byKey: make(map[registryKey]any)}
}

// Register associates a (kind, baseURL) pair with an Outbound. Last write wins.
func (r *Registry) Register(kind catalog.ProviderKind, baseURL string, o Outbound) {
	r.byKey[registryKey{Kind: kind, BaseURL: baseURL}] = o
}

// RegisterMessages associates a (kind, baseURL) pair with a MessagesOutbound.
// Last write wins. Separate from Register so the type system is clear at call sites.
func (r *Registry) RegisterMessages(kind catalog.ProviderKind, baseURL string, o MessagesOutbound) {
	r.byKey[registryKey{Kind: kind, BaseURL: baseURL}] = o
}

// Get returns the Outbound for a (kind, baseURL) pair, or an error if none registered.
func (r *Registry) Get(kind catalog.ProviderKind, baseURL string) (Outbound, error) {
	v, ok := r.byKey[registryKey{Kind: kind, BaseURL: baseURL}]
	if !ok {
		return nil, fmt.Errorf("provider: no outbound registered for kind %q baseURL %q", kind, baseURL)
	}
	o, ok := v.(Outbound)
	if !ok {
		return nil, fmt.Errorf("provider: registered client for kind %q baseURL %q does not implement Outbound", kind, baseURL)
	}
	return o, nil
}

// GetMessages returns the MessagesOutbound for a (kind, baseURL) pair, or an error if none registered.
func (r *Registry) GetMessages(kind catalog.ProviderKind, baseURL string) (MessagesOutbound, error) {
	v, ok := r.byKey[registryKey{Kind: kind, BaseURL: baseURL}]
	if !ok {
		return nil, fmt.Errorf("provider: no outbound registered for kind %q baseURL %q", kind, baseURL)
	}
	o, ok := v.(MessagesOutbound)
	if !ok {
		return nil, fmt.Errorf("provider: registered client for kind %q baseURL %q does not implement MessagesOutbound", kind, baseURL)
	}
	return o, nil
}
