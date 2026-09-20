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
	"net/http"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/routing"
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
