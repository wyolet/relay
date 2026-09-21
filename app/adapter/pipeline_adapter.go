package adapter

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// PipelineAdapter returns a pipeline.Adapter backed by this spec's upstream
// path and auth strategy. The returned value is safe for concurrent use.
func (s *Spec) PipelineAdapter() pipeline.Adapter {
	return &specAdapter{spec: s}
}

// specAdapter implements pipeline.Adapter for a Spec.
type specAdapter struct {
	spec *Spec
}

var _ pipeline.Adapter = (*specAdapter)(nil)

// Call issues POST {baseURL}{path}: the host's own path when set (hostPath
// non-nil, verbatim — an explicit "" appends nothing), else the shape default
// (spec.UpstreamPathFn / spec.DefaultPath). Auth headers are set per spec.Auth
// (or spec.OAuthAuth when oauth is true and the spec defines an OAuth
// variant); forwarded headers are applied first so Relay's own headers win on
// conflict.
func (a *specAdapter) Call(ctx context.Context, baseURL string, hostPath *string, apiKey string, body []byte, hdr http.Header, upstreamModel string, stream, oauth bool) (*http.Response, error) {
	path := a.spec.DefaultPath
	if a.spec.UpstreamPathFn != nil {
		path = a.spec.UpstreamPathFn(upstreamModel, stream)
	}
	if hostPath != nil {
		path = *hostPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	req.Header.Set("Content-Type", "application/json")

	auth := a.spec.Auth
	if oauth && a.spec.OAuthAuth.Header != "" {
		auth = a.spec.OAuthAuth
	}

	if apiKey != "" && auth.Header != "" {
		val := apiKey
		if auth.Scheme != "" {
			val = auth.Scheme + " " + apiKey
		}
		req.Header.Set(auth.Header, val)
	}

	for k, v := range auth.ExtraHeaders {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}

	return a.spec.client.Do(req)
}

// ExtractTokens delegates to the spec's extractor, or returns nil if unset.
func (a *specAdapter) ExtractTokens(body []byte) pkgusage.Tokens {
	if a.spec.ExtractTokens == nil {
		return nil
	}
	return a.spec.ExtractTokens(body)
}

// Retryable classifies upstream HTTP responses for the pipeline retry loop.
// Classification is uniform across specs: 401/403→auth, 429→rate-limit,
// 500-599→server error. Any spec that needs different classification can
// override by wrapping the returned pipeline.Adapter.
func (a *specAdapter) Retryable(resp *http.Response) (retry bool, kind keypool.FailureKind, retryAfter time.Duration) {
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
