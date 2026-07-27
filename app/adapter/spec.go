// Package adapter provides the generic adapter framework: a Spec type that
// bundles wire-shape metadata (inbound URL paths, upstream URL path, auth
// strategy, canonical translator, token extractor), a generic pipeline.Adapter
// implementation parameterised by path and auth strategy, a Registry of Specs,
// and a generic route mounter that iterates registered specs.
//
// This package is the composition root's extension point: to add a new wire
// shape, add one Spec to the registry in cmd/relay/main.go. No branching in
// dispatch; no per-shape packages inside app/.
//
// Deliberately out of scope:
//   - Provider-catalog data (lives in wyolet/relay-catalog)
//   - The canonical v1.Translator interface (lives in pkg/relay/v1)
//   - The OLD app/adapters.Translator interface (deleted in PR 5)
package adapter

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/metrics"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// AuthStrategy configures how an adapter authenticates to the upstream.
type AuthStrategy struct {
	// Header is the HTTP header to set (e.g. "Authorization", "x-api-key").
	Header string

	// Scheme is prepended to the key with a space if non-empty
	// (e.g. "Bearer" → "Bearer sk-..."). Empty means no prefix.
	Scheme string

	// ExtraHeaders are static header key/value pairs added unconditionally
	// (e.g. {"anthropic-version": "2023-06-01"}). Only applied when the
	// header is not already present in the forwarded request headers.
	ExtraHeaders map[string]string
}

// Spec describes one inbound wire shape and its upstream call semantics.
// One Spec registration = one inbound URL surface + one upstream path.
//
// The three OpenAI shapes (CC, Responses, Embeddings) are three Specs.
// The Anthropic shape is one Spec. A future Gemini shape would be one Spec.
type Spec struct {
	// Name is the wire-protocol identifier, matching adapters.Name values.
	// Used as the key in the Registry and as the DispatchInput.Inbound value.
	Name adapters.Name

	// InboundPaths is the set of inbound HTTP paths this spec owns.
	// Each entry is registered as a POST route via the generic mounter.
	// Example: ["/v1/chat/completions", "/openai/v1/chat/completions"].
	InboundPaths []InboundPath

	// DefaultPath is the shape-canonical upstream path, used when the host
	// doesn't set its own (Host.Spec.Path, which wins verbatim — including
	// an explicit ""). Example: "/v1/chat/completions", "/v1/responses".
	// Ignored when UpstreamPathFn is set.
	DefaultPath string

	// UpstreamPathFn resolves the upstream path per request from the upstream
	// model name and the sync/stream choice. Set only for shapes that encode
	// the model and/or stream selection in the URL rather than the body —
	// Gemini's "/v1beta/models/{model}:generateContent" vs
	// ":streamGenerateContent". When nil, DefaultPath is used verbatim.
	UpstreamPathFn func(upstreamModel string, stream bool) string

	// Auth configures how the upstream is authenticated with an API-key
	// credential (the default).
	Auth AuthStrategy

	// OAuthAuth is the alternate auth used when the resolved credential is an
	// OAuth token (HostKey value-kind "oauth") rather than an API key — e.g.
	// Anthropic subscription tokens go out as `Authorization: Bearer …` + the
	// oauth beta header instead of `x-api-key`. A zero value (empty Header)
	// means the spec has no OAuth variant and Auth is used for every credential.
	// Selecting it keeps the binding on the same wire shape (so same-shape
	// byte-pass still applies); only the upstream auth headers differ.
	OAuthAuth AuthStrategy

	// Translator is the canonical v1.Translator for this shape. Used by
	// the standard dispatch chain (inbound→canonical→upstream and back).
	// Nil means this shape has no canonical translator (e.g. byte-pass-only).
	Translator v1.Translator

	// BytePass signals that this shape is always byte-equivalent with its
	// upstream (no cross-shape translation is ever needed). Embeddings is
	// the canonical example: it's a direct passthrough to any OpenAI-compat
	// host and there is no canonical embeddings type.
	//
	// When true, Translator is unused even if set.
	BytePass bool

	// ExtractTokens extracts usage tokens from the upstream response body.
	// If nil, the adapter returns nil tokens (no usage tracking).
	ExtractTokens func(body []byte) pkgusage.Tokens

	// UseHTTP1 disables HTTP/2 negotiation on the upstream transport.
	// Necessary for endpoints that trigger Go's HTTP/2 client bugs (e.g.
	// OpenAI /v1/responses sends GOAWAY mid-request over HTTP/2).
	UseHTTP1 bool

	// ParamPaths maps canonical sampling-param names ("temperature",
	// "top_p", "top_k") to their dot-separated location in this shape's
	// request JSON (e.g. "temperature" top-level for the OpenAI/Anthropic
	// shapes, "generationConfig.temperature" for Gemini). Used by the
	// byte-pass dispatch to strip params a routed model declares
	// unsupported without invoking the translator; params absent from the
	// map cannot be stripped on the byte-pass path. Wire-shape knowledge,
	// so it lives here on the Spec literal (composition root), not in
	// dispatch code.
	ParamPaths map[string]string

	// IsNativePath reports whether the resolved routing plan implies that the
	// upstream host natively speaks this inbound shape — making byte-pass to
	// this spec's UpstreamPath the correct strategy, regardless of whether
	// the host's HostBinding.Adapter matches the inbound shape Name.
	//
	// The canonical use case: OpenAIResponses inbound. The spec's Name is
	// "openai_responses" but the host's HostBinding.Adapter is "openai".
	// OpenAI-proper hosts speak the Responses API natively, so when this
	// predicate returns true (host Name == "openai") the dispatch byte-passes
	// to /v1/responses via this spec's adapter. Non-openai hosts return false
	// and dispatch falls through to the canonical cross-shape chain.
	//
	// For shapes where the inbound Name equals the binding Adapter (CC, Anthropic,
	// Embeddings), this field is nil — the standard Name-equality check handles it.
	IsNativePath func(plan *routing.Plan) bool

	// client is the shared *http.Client for this spec's pipeline.Adapter.
	// Populated by Build.
	client *http.Client
}

// InboundPath describes one inbound HTTP route for a spec.
type InboundPath struct {
	// Path is the URL path, e.g. "/v1/chat/completions".
	Path string

	// OperationID is the huma operation ID, e.g. "chat_completions".
	OperationID string

	// Summary is the huma operation summary.
	Summary string
}

const (
	defaultTimeout = 5 * time.Minute

	// defaultMaxIdleConnsPerHost keeps hot upstream connections warm. The
	// stdlib default (2) re-dials nearly every request at high per-host RPS.
	// Overridable per deployment via SetUpstreamMaxIdleConnsPerHost.
	defaultMaxIdleConnsPerHost = 128

	// maxIdleConnsScale gives the total idle pool headroom over the per-host
	// cap so several hot hosts can each keep a full keep-alive pool.
	maxIdleConnsScale = 8

	idleConnTimeout       = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// maxIdleConnsPerHost is the per-host idle-connection ceiling applied to
// every upstream transport built after it is set. Read at Build time.
var maxIdleConnsPerHost = defaultMaxIdleConnsPerHost

// SetUpstreamMaxIdleConnsPerHost overrides the per-host idle-connection cap
// used by every Spec built afterwards (the composition root wires the
// RELAY_UPSTREAM_MAX_IDLE_PER_HOST value here). Values < 1 are ignored so a
// zero config default leaves the built-in 128. Not safe to call concurrently
// with Build — invoke once at boot, before specs are constructed.
func SetUpstreamMaxIdleConnsPerHost(n int) {
	if n >= 1 {
		maxIdleConnsPerHost = n
	}
}

// NewUpstreamTransport builds the tuned upstream transport. http1 empties
// TLSNextProto on the same tuned base to disable HTTP/2 negotiation for
// shapes that trip Go's HTTP/2 client bugs (see Spec.UseHTTP1). Exported for
// the composition root, which applies the same pooling to the proxy runner's
// client (its upstreams are just as hot as the pipeline's).
//
// The returned RoundTripper wraps the transport with connection-reuse
// accounting (relay_upstream_connections_total) — the tripwire for the
// MaxIdleConnsPerHost-style churn this pooling exists to prevent.
func NewUpstreamTransport(http1 bool) http.RoundTripper {
	perHost := maxIdleConnsPerHost
	tr := &http.Transport{
		MaxIdleConns:          perHost * maxIdleConnsScale,
		MaxIdleConnsPerHost:   perHost,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
	if http1 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return connTrackingTransport{base: tr}
}

// connTrackingTransport counts whether each upstream attempt got a fresh
// dial or a pooled connection. httptrace composes with any trace already
// on the context, so this is transparent to callers.
type connTrackingTransport struct {
	base http.RoundTripper
}

var connTrace = &httptrace.ClientTrace{
	GotConn: func(info httptrace.GotConnInfo) { metrics.UpstreamConn(info.Reused) },
}

func (t connTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(
		req.WithContext(httptrace.WithClientTrace(req.Context(), connTrace)))
}

// Build finalises the Spec by constructing its shared HTTP client. The client
// pools upstream connections via a tuned transport (per-host idle ceiling from
// SetUpstreamMaxIdleConnsPerHost) and keeps the 5-minute client timeout for
// long streamed responses. Must be called once after all fields are set,
// before the spec is added to a Registry. Returns s for chaining.
func (s *Spec) Build() *Spec {
	s.client = &http.Client{
		Timeout:   defaultTimeout,
		Transport: NewUpstreamTransport(s.UseHTTP1),
	}
	return s
}

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
