// Package openai implements app/pipeline.Adapter for the OpenAI wire shape.
// Ollama is also covered because it exposes an OpenAI-compatible endpoint at
// the same /v1/chat/completions path.
package openai

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// http1Transport disables HTTP/2 negotiation. OpenAI's /v1/responses endpoint
// returns GOAWAY frames mid-request over HTTP/2 from Go's stdlib client,
// causing "unexpected EOF" errors; /v1/chat/completions is unaffected.
// Forcing HTTP/1.1 sidesteps the issue. Track as a follow-up: the underlying
// cause may be in net/http's HTTP/2 stack or in OpenAI's edge.
func http1Transport() *http.Transport {
	return &http.Transport{
		TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// compile-time interface check
var _ pipeline.Adapter = (*Adapter)(nil)

const (
	chatPath       = "/v1/chat/completions"
	responsesPath  = "/v1/responses"
	embeddingsPath = "/v1/embeddings"
	defaultTimeout = 5 * time.Minute
)

// Option configures an Adapter.
type Option func(*Adapter)

// WithClient replaces the default HTTP client. Useful in tests.
func WithClient(c *http.Client) Option {
	return func(a *Adapter) { a.client = c }
}

// WithPath overrides the upstream POST path. Defaults to /v1/chat/completions.
// Use responsesPath for the Responses API adapter; embeddingsPath for the
// Embeddings API adapter.
func WithPath(p string) Option {
	return func(a *Adapter) { a.path = p }
}

// Adapter implements pipeline.Adapter for OpenAI (and Ollama).
type Adapter struct {
	client *http.Client
	path   string
}

// New returns a ready-to-use Adapter. The default HTTP client has a 5-minute
// timeout, matching the legacy internal/provider/openai/client.go behaviour.
// HTTP/2 is disabled — see http1Transport.
func New(opts ...Option) *Adapter {
	a := &Adapter{
		client: &http.Client{Timeout: defaultTimeout, Transport: http1Transport()},
		path:   chatPath,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Call issues POST {baseURL}{path} with the supplied body and API key.
// Any headers in hdr are forwarded as-is before the Relay-specific headers
// are set (so the caller can forward client headers without overriding
// Authorization).
func (a *Adapter) Call(ctx context.Context, baseURL, apiKey string, body []byte, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+a.path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Copy forwarded headers first so Relay's own headers win on conflict.
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	return a.client.Do(req)
}

// ExtractTokens delegates to the pure pkg/api/openai extractor. For streaming
// responses the pipeline tees the full body, so the final SSE chunk (which
// carries the usage block) is included in body.
func (a *Adapter) ExtractTokens(body []byte) pkgusage.Tokens {
	return pkgopenai.ExtractTokens(body)
}

// Retryable classifies upstream HTTP responses for the pipeline retry loop.
//
//   - 401, 403  → auth failure, rotate key
//   - 429       → rate-limited; use Retry-After to distinguish short/long
//   - 500-599   → server error, retry on next key
//   - everything else (including non-retryable 4xx) → no retry
func (a *Adapter) Retryable(resp *http.Response) (retry bool, kind keypool.FailureKind, retryAfter time.Duration) {
	if resp == nil {
		return false, 0, 0
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return true, keypool.FailureAuth, 0
	case resp.StatusCode == http.StatusTooManyRequests:
		ra := pipeline.RetryAfterHeader(resp.Header)
		k := keypool.FailureRateLimitShort
		if ra > 5*time.Second {
			k = keypool.FailureRateLimitLong
		}
		return true, k, ra
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return true, keypool.FailureServerError, 0
	default:
		return false, 0, 0
	}
}
