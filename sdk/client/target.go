package client

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/wyolet/relay/sdk/catalog"
)

// Target is a fully-resolved, self-consistent upstream call config.
type Target struct {
	baseURL  string
	adapter  Adapter
	upstream string
	binding  catalog.Binding
}

func targetFromBinding(b catalog.Binding, h catalog.Host) (Target, error) {
	a, ok := AdapterByName(b.Adapter)
	if !ok {
		return Target{}, fmt.Errorf("relay client: unknown adapter %q", b.Adapter)
	}
	return Target{
		baseURL:  strings.TrimRight(h.BaseURL, "/"),
		adapter:  a,
		upstream: b.Upstream,
		binding:  b,
	}, nil
}

func (t Target) withBaseURL(url string) Target {
	t.baseURL = strings.TrimRight(url, "/")
	return t
}

func (t Target) withAdapterName(name string) (Target, error) {
	a, ok := AdapterByName(name)
	if !ok {
		return Target{}, fmt.Errorf("relay client: unknown adapter %q", name)
	}
	t.adapter = a
	return t, nil
}

func (t Target) withUpstreamModel(name string) Target {
	t.upstream = name
	return t
}

func (t Target) client(apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL:   t.baseURL,
		apiKey:    apiKey,
		http:      http.DefaultClient,
		transport: httpTransport{},
		target:    t,
	}
	t.adapter.apply(c)
	for _, o := range opts {
		o(c)
	}
	return c
}
