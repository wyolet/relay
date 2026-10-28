package adapter

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/pkg/reqbody"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// PipelineAdapter returns a pipeline.Adapter backed by this spec's upstream
// path and auth strategy. The returned value is safe for concurrent use.
//
// A spec declaring a CountPath gets the variant that also satisfies TokenCounter, so a caller's type assertion answers "does this upstream count tokens for us" without consulting the spec.
func (s *Spec) PipelineAdapter() pipeline.Adapter {
	base := &specAdapter{spec: s}
	if s.CountPath != "" {
		return &countingSpecAdapter{specAdapter: base}
	}
	return base
}

// specAdapter implements pipeline.Adapter for a Spec.
type specAdapter struct {
	spec *Spec
}

var (
	_ pipeline.Adapter            = (*specAdapter)(nil)
	_ pipeline.ResponseSummarizer = (*specAdapter)(nil)
	_ pipeline.CallTimeouter      = (*specAdapter)(nil)
)

// Call issues POST {baseURL}{path}: the host's own path when set (hostPath
// non-nil, verbatim — an explicit "" appends nothing), else the shape default
// (spec.UpstreamPathFn / spec.DefaultPath). Auth headers are set per spec.Auth
// (or spec.OAuthAuth when oauth is true and the spec defines an OAuth
// variant); forwarded headers are applied first so Relay's own headers win on
// conflict.
func (a *specAdapter) Call(ctx context.Context, baseURL string, hostPath *string, apiKey string, body []byte, hdr http.Header, upstreamModel string, stream, oauth bool) (*http.Response, error) {
	path := a.spec.DefaultPath
	if a.spec.UpstreamPathFn != nil {
		path = a.spec.UpstreamPathFn(pathSegment(upstreamModel), stream)
	}
	if hostPath != nil {
		path = *hostPath
	}
	callCtx, cancel := a.callContext(ctx, stream)
	req, releaseBody, err := a.newRequest(callCtx, baseURL+path, apiKey, body, hdr, oauth)
	if err != nil {
		cancelIfSet(cancel)
		return nil, err
	}

	resp, err := a.spec.client.Do(req)
	releaseBody()
	if err != nil {
		cancelIfSet(cancel)
		return nil, err
	}
	resp.Body = a.bindBody(resp.Body, cancel, stream)
	return resp, nil
}

// pathSegment escapes the upstream model so it stays one URL path segment. A wildcard alias forwards the caller's raw model string, so without this it could add segments or start a query on the operator's credential. A bare dot segment is escaped too, since servers resolve it.
func pathSegment(model string) string {
	if model == "." || model == ".." {
		return strings.ReplaceAll(model, ".", "%2E")
	}
	return url.PathEscape(model)
}

// newRequest builds one POST to url carrying body: the caller's forwarded headers first, then relay's own, so a caller can never override the upstream credential or the content type. releaseBody must be called once Do returns, so the transport stops pinning body while the response streams.
func (a *specAdapter) newRequest(ctx context.Context, url, apiKey string, body []byte, hdr http.Header, oauth bool) (req *http.Request, releaseBody func(), err error) {
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, nil, err
	}
	releaseBody = reqbody.Attach(req, body)

	// A copy per attempt: failover reuses hdr, and the auth headers set below differ per host.
	if hdr != nil {
		req.Header = hdr.Clone()
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
	return req, releaseBody, nil
}

// ExtractTokens delegates to the spec's extractor, or returns nil if unset.
func (a *specAdapter) ExtractTokens(body []byte) pkgusage.Tokens {
	if a.spec.ExtractTokens == nil {
		return nil
	}
	return a.spec.ExtractTokens(body)
}

// SummarizeResponse decodes a buffered upstream body's Summary with the spec's translator. false means the spec has no translator to read it with, or the translator panicked on the body: post-flight runs on its own goroutine, so a panic there would take the process down for one malformed response.
func (a *specAdapter) SummarizeResponse(body []byte) (sum v1.Summary, ok bool) {
	if a.spec.Translator == nil {
		return v1.Summary{}, false
	}
	defer func() {
		if v := recover(); v != nil {
			slog.Error("adapter: response summary panic recovered", "adapter", a.spec.Name, "panic", v)
			sum, ok = v1.Summary{}, false
		}
	}()
	sum, err := v1.ExtractSummary(a.spec.Translator, body)
	if err != nil {
		// The body did not decompress: no decoder can read it, so it summarizes as empty, as the usage hook would find it.
		return v1.Summary{}, true
	}
	return sum, true
}

// Retryable classifies upstream HTTP responses for the pipeline retry loop.
// Classification is uniform across specs: 401→auth, 403→forbidden,
// 429→rate-limit, 500-599→server error. Any spec that needs different
// classification can override by wrapping the returned pipeline.Adapter.
func (a *specAdapter) Retryable(resp *http.Response) (retry bool, kind keypool.FailureKind, retryAfter time.Duration) {
	if resp == nil {
		return false, 0, 0
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return true, keypool.FailureAuth, 0
	case resp.StatusCode == http.StatusForbidden:
		return true, keypool.FailureForbidden, 0
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
