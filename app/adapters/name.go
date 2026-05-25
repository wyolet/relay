// Package adapters names the wire protocol the relay speaks to an upstream.
//
// One Name corresponds to one `app/api/<name>` package (Relay-side glue)
// and one `pkg/api/<name>` package (pure, vendorable shape parsers).
// Hosts can serve models that speak different adapters — e.g. AWS Bedrock
// hosts Claude (Anthropic) and Llama (OpenAI-shape). The dispatch key
// therefore lives on the per-Model HostBinding, not on the Host itself.
//
// Capabilities (streaming, vision, function-calling, etc.) live on
// Model.Spec.Capabilities; an adapter's job is wire format, not feature
// negotiation. A future "adapter framework" may bundle capability
// expectations per adapter Name, but today the two concerns are separate.
package adapters

// Name is the wire-protocol name.
type Name string

const (
	// OpenAI: the OpenAI Chat Completions and Models shape. Also used for
	// Ollama, Together, Groq, Fireworks, Azure OpenAI, and any vendor
	// fronting an OpenAI-compatible endpoint.
	OpenAI Name = "openai"

	// Anthropic: the Anthropic Messages shape. Also used for Anthropic via
	// AWS Bedrock (Claude on Bedrock keeps the Anthropic shape) and via
	// GCP Vertex.
	Anthropic Name = "anthropic"

	// Gemini: the Google Gemini native shape (POST /v1beta/models/{model}:
	// generateContent). Unique among registered shapes: the upstream model
	// name and the sync/stream choice live in the URL path, not the request
	// body, so the Spec resolves its upstream path per request via
	// UpstreamPathFn. Also used for Gemini via GCP Vertex.
	Gemini Name = "gemini"

	// OpenAIResponses is the OpenAI Responses API shape (POST /v1/responses).
	// Distinct from OpenAI (Chat Completions) because the upstream path differs.
	// Phase 1: byte-passthrough only; no cross-shape translation.
	OpenAIResponses Name = "openai_responses"

	// OpenAIEmbeddings is the OpenAI Embeddings API shape (POST /v1/embeddings).
	// Distinct from OpenAI (Chat Completions) because the upstream path differs.
	// Supported by any OpenAI-compatible host (Voyage, Together, Fireworks,
	// Cohere compat, etc.). Phase 1: byte-passthrough only.
	OpenAIEmbeddings Name = "openai_embeddings"

	// Canonical is relay's own wire shape (pkg/relay/v1), served at /v1/*.
	// It is an inbound-only shape: callers POST canonical and relay routes,
	// translates canonical→upstream-vendor, and returns canonical. There is
	// no canonical upstream, so it never appears as a HostBinding.Adapter.
	Canonical Name = "canonical"
)

// Valid reports whether n is one of the supported adapter names.
func (n Name) Valid() bool {
	switch n {
	case OpenAI, Anthropic, Gemini, OpenAIResponses, OpenAIEmbeddings:
		return true
	}
	return false
}

// All returns every supported Name. Stable order: useful for tests and
// CLI flag help text. Order does not imply preference.
func All() []Name {
	return []Name{OpenAI, Anthropic, Gemini, OpenAIResponses, OpenAIEmbeddings}
}
