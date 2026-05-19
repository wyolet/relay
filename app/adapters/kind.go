// Package adapter names the wire protocol the relay speaks to an upstream.
//
// One Kind corresponds to one `app/api/<kind>` package (Relay-side glue)
// and one `pkg/api/<kind>` package (pure, vendorable shape parsers).
// Hosts can serve models that speak different adapters — e.g. AWS Bedrock
// hosts Claude (Anthropic) and Llama (OpenAI-shape). The dispatch key
// therefore lives on the per-Model HostBinding, not on the Host itself.
//
// Capabilities (streaming, vision, function-calling, etc.) live on
// Model.Spec.Capabilities; an adapter's job is wire format, not feature
// negotiation. A future "adapter framework" may bundle capability
// expectations per adapter Kind, but today the two concerns are separate.
package adapters

// Kind is the wire-protocol identifier.
type Kind string

const (
	// OpenAI: the OpenAI Chat Completions and Models shape. Also used for
	// Ollama, Together, Groq, Fireworks, Azure OpenAI, and any vendor
	// fronting an OpenAI-compatible endpoint.
	OpenAI Kind = "openai"

	// Anthropic: the Anthropic Messages shape. Also used for Anthropic via
	// AWS Bedrock (Claude on Bedrock keeps the Anthropic shape) and via
	// GCP Vertex.
	Anthropic Kind = "anthropic"
)

// Valid reports whether k is one of the supported adapter Kinds.
func (k Kind) Valid() bool {
	switch k {
	case OpenAI, Anthropic:
		return true
	}
	return false
}

// All returns every supported Kind. Stable order: useful for tests and
// CLI flag help text. Order does not imply preference.
func All() []Kind { return []Kind{OpenAI, Anthropic} }
