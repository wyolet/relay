package pipeline

import (
	"io"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/keypool"
	"github.com/wyolet/relay/internal/provider"
	"github.com/wyolet/relay/internal/ratelimit"
	"github.com/wyolet/relay/internal/usage"
)

// Request carries everything Run needs for one request.
//
// Transport-extracted fields (Body, Attribution, PassthroughAuth) are set by
// the HTTP handler from the inbound *http.Request; domain config fields
// (Provider, Policy, Secrets, etc.) come from the resolved routing.RequestPlan.
//
// Keeping both here removes the transport.Channel from the public API so that
// callers treat Run as a pure function: in → out, no shared channel plumbing.
type Request struct {
	// --- transport-extracted (set by HTTP handler) ---

	// Body is the raw request body to forward upstream.
	Body []byte

	// Attribution is the per-request metadata map (from X-Relay-Metadata header
	// or parsed request body). Nil is valid; pipeline falls back to
	// reqid.Attribution(ctx).
	Attribution map[string]string

	// PassthroughAuth, when non-empty, enables passthrough mode: key selection is
	// skipped and this value is forwarded verbatim as the upstream Authorization
	// header. sha256[:12] of the value is used as the key_hash metric label.
	// The raw value is never logged.
	PassthroughAuth string

	// PassthroughHeaders are inbound headers (anthropic-beta, user-agent, x-app,
	// x-claude-code-session-id, etc.) that must travel to the upstream verbatim.
	// Captured by the transport layer; injected into the provider call via
	// provider-specific context extras (e.g. anthropic.WithRequestExtras).
	PassthroughHeaders map[string]string

	// --- domain config (set from routing.RequestPlan by the HTTP handler) ---

	Provider    *catalog.Provider
	Policy      *catalog.Policy
	Model       *catalog.Model
	Secrets     []*catalog.Secret
	Selector    *keypool.Selector
	Outbound    provider.Outbound
	DoUpstream  UpstreamFunc // overrides Outbound when non-nil
	MaxAttempts int          // 0 → 3

	// Limiter and Rules enable rate limiting. If either is nil/empty, rate
	// limiting is skipped.
	Limiter *ratelimit.Limiter
	Rules   []catalog.ResolvedRule

	// CatalogStore, when non-nil, is used to look up effective pricing and
	// per-key (secret-attached) rate-limit rules.
	CatalogStore catalog.Store

	// TokenExtractor, when non-nil, is called on each response chunk body.
	TokenExtractor func(chunk []byte) usage.Tokens

	// InboundAdapter and UpstreamAdapter enable cross-shape transform.
	InboundAdapter  TransformAdapter
	UpstreamAdapter TransformAdapter
}

// Response is the typed result of a Run call.
//
// Body is an io.Reader the caller must drain (or close if it implements
// io.Closer). The HTTP handler is responsible for all wire framing: setting
// the HTTP status code, forwarding Headers to the client, and SSE framing.
type Response struct {
	// Status is the HTTP status code to send to the client.
	Status int

	// Headers carries domain-meaningful response headers for the transport layer.
	// Populated fields:
	//   "Content-Type" — upstream Content-Type (application/json or text/event-stream).
	//   "Retry-After"  — present on 429 responses; value in whole seconds as a string.
	// Internal pipeline signals (X-Relay-Status, X-Relay-Final) are NOT included.
	Headers map[string]string

	// Body is the response body. The caller must read it to completion (or close it).
	// For error responses this is a complete JSON error envelope.
	// For non-streaming success this is the complete upstream body.
	// For streaming success this streams upstream bytes (SSE) as they arrive.
	Body io.Reader

	// UpstreamDuration is the wall-clock time spent in the last upstream HTTP
	// call that was actually served. Zero means the request never reached upstream
	// (rate-limited, no healthy keys, etc.).
	UpstreamDuration time.Duration
}
