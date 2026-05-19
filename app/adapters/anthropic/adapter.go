// Package anthropic implements app/pipeline.Adapter for the Anthropic Messages
// wire shape. One instance is shared across all requests; Call is goroutine-safe.
package anthropic

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	pkganthropic "github.com/wyolet/relay/pkg/adapters/anthropic"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// compile-time interface check
var _ pipeline.Adapter = (*Adapter)(nil)

const (
	messagesPath      = "/v1/messages"
	defaultTimeout    = 5 * time.Minute
	defaultAPIVersion = "2023-06-01"
)

// Option configures an Adapter.
type Option func(*Adapter)

// WithClient replaces the default HTTP client. Useful in tests.
func WithClient(c *http.Client) Option {
	return func(a *Adapter) { a.client = c }
}

// Adapter implements pipeline.Adapter for the Anthropic Messages API.
type Adapter struct {
	client *http.Client
}

// New returns a ready-to-use Adapter with a 5-minute HTTP client timeout.
func New(opts ...Option) *Adapter {
	a := &Adapter{
		client: &http.Client{Timeout: defaultTimeout},
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Call issues POST {baseURL}/v1/messages with Anthropic auth headers. Any
// headers in hdr are copied first so Relay's own headers win on conflict.
// anthropic-version defaults to "2023-06-01" when absent from hdr.
func (a *Adapter) Call(ctx context.Context, baseURL, apiKey string, body []byte, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+messagesPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Copy forwarded headers first so Relay's own headers win on conflict.
	// This propagates anthropic-beta, User-Agent, X-App, X-Claude-Code-Session-Id,
	// X-Stainless-*, and any other headers the handler decided to forward.
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	// Default anthropic-version if the caller did not forward one.
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", defaultAPIVersion)
	}

	return a.client.Do(req)
}

// ExtractTokens delegates to the pure pkg/api/anthropic extractor. The
// pipeline tees the full body, so SSE final-event usage blocks are included.
func (a *Adapter) ExtractTokens(body []byte) pkgusage.Tokens {
	return pkganthropic.ExtractTokens(body)
}

// Retryable classifies upstream HTTP responses for the pipeline retry loop.
//
//   - 401, 403       → auth failure, rotate key
//   - 429            → rate-limited; Retry-After distinguishes short/long
//   - 500-599 + 529  → server/overloaded error, retry on next key
//   - everything else → no retry
//   - nil resp       → no retry
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
		// 529 (Anthropic "overloaded") falls in this range automatically.
		return true, keypool.FailureServerError, 0
	default:
		return false, 0, 0
	}
}
