package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// transport is how a serialized request reaches the target and how the
// response bytes come back. It is orthogonal to the translator (which
// owns wire shape): the same canonical request serializes once, then a
// transport carries the bytes over HTTP (default) or one multiplexed
// WebSocket (wsTransport). The returned body is the raw response stream —
// an SSE event sequence when streaming, a single JSON object otherwise —
// so Generate / GenerateStream stay transport-agnostic.
type transport interface {
	// path is resolved per call (it may depend on the model / stream mode,
	// e.g. Gemini's URL-path-encoded model) so a shared Client stays race-free.
	roundTrip(ctx context.Context, c *Client, path string, body []byte) (*rtResponse, error)
	// Close releases any persistent resources (a WebSocket connection).
	// The default HTTP transport is a no-op.
	Close() error
}

// rtResponse is the transport-neutral response: the status and a body the
// caller must Close. Closing triggers connection release / reuse.
type rtResponse struct {
	status int
	body   io.ReadCloser
}

// httpTransport is the default: one HTTP POST per request.
type httpTransport struct{}

func (httpTransport) roundTrip(ctx context.Context, c *Client, path string, body []byte) (*rtResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	c.applyAuth(httpReq.Header)
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	return &rtResponse{status: resp.StatusCode, body: resp.Body}, nil
}

func (httpTransport) Close() error { return nil }

// applyAuth attaches the configured API key to a header set. Shared by the
// HTTP transport (per request) and the WS transport (on the upgrade dial).
func (c *Client) applyAuth(h http.Header) {
	if c.apiKey == "" || c.auth.Header == "" {
		return
	}
	val := c.apiKey
	if c.auth.Scheme != "" {
		val = c.auth.Scheme + " " + c.apiKey
	}
	h.Set(c.auth.Header, val)
}
