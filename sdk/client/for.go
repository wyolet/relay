package client

import (
	"fmt"

	"github.com/wyolet/relay/sdk/catalog"
)

// TargetOption overrides one whole piece of a catalog-resolved Target before
// the client is built. Each option yields a consistent config — path and
// translator are only ever swapped via an Adapter.
type TargetOption func(*Target) error

// WithBaseURL overrides the resolved host base URL (proxy/gateway). Adapter
// and auth are unchanged.
func WithBaseURL(url string) TargetOption {
	return func(t *Target) error {
		*t = t.withBaseURL(url)
		return nil
	}
}

// WithAdapterName swaps the whole wire bundle (translator + path + auth).
func WithAdapterName(name string) TargetOption {
	return func(t *Target) error {
		updated, err := t.withAdapterName(name)
		if err != nil {
			return err
		}
		*t = updated
		return nil
	}
}

// WithUpstreamModel overrides the wire model name sent upstream.
func WithUpstreamModel(name string) TargetOption {
	return func(t *Target) error {
		*t = t.withUpstreamModel(name)
		return nil
	}
}

// WithClient attaches client Options (custom *http.Client, extra headers,
// sync timeout, ...) to a catalog-resolved client. They apply over the
// adapter's defaults at construction.
func WithClient(opts ...Option) TargetOption {
	return func(t *Target) error {
		t.clientOpts = append(t.clientOpts, opts...)
		return nil
	}
}

// For resolves a model ref against the embedded catalog and returns a wired
// client. ref forms: "gpt-4o", "openai/gpt-4o", "gpt-4o@openai-direct".
func For(ref, apiKey string, opts ...TargetOption) (*Client, error) {
	cat, err := catalog.Load()
	if err != nil {
		return nil, fmt.Errorf("relay client: catalog: %w", err)
	}
	binding, host, err := cat.Resolve(ref)
	if err != nil {
		return nil, err
	}
	t, err := targetFromBinding(binding, host)
	if err != nil {
		return nil, err
	}
	for _, o := range opts {
		if err := o(&t); err != nil {
			return nil, err
		}
	}
	return t.client(apiKey), nil
}
