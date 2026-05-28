package adapters

import (
	"github.com/wyolet/relay/sdk/adapters/openai"
)

// Translator converts between an adapter's native wire shape and OpenAI's
// shape (the canonical hub). Each non-OpenAI adapter implements all six
// methods; the OpenAI adapter uses Identity which is a no-op.
//
// The four sync methods are stateless. The two stream methods return a
// stateful per-chunk function — they hold parser state across the SSE
// stream (e.g. accumulating message_start → content_block_delta → ...).
// A nil return from the stream factories means "no transform needed"
// (identity); callers should pass chunks straight through.
//
// Lossy fields are tracked in docs/adapters.md and tagged in transform
// implementations with `// canonical:` comments.
type Translator interface {
	// ParseRequest parses an inbound request body in this adapter's
	// native shape and returns its OpenAI-shape equivalent.
	ParseRequest(body []byte) (*openai.FullChatRequest, error)

	// SerializeRequest takes an OpenAI-shape request and emits this
	// adapter's native wire body. Used to translate inbound shape →
	// upstream shape before the HTTP call.
	SerializeRequest(req *openai.FullChatRequest) ([]byte, error)

	// ParseResponse parses an upstream response body in this adapter's
	// native shape and returns its OpenAI-shape equivalent.
	ParseResponse(body []byte) (*openai.ChatResponse, error)

	// SerializeResponse takes an OpenAI-shape response and emits this
	// adapter's native wire body. Used to translate upstream shape →
	// inbound shape on the response leg.
	SerializeResponse(resp *openai.ChatResponse) ([]byte, error)

	// NewToOpenAIStream returns a stateful function that converts a
	// single SSE chunk from this adapter's stream shape into OpenAI's
	// stream shape. Returns nil when no transform is needed (identity).
	NewToOpenAIStream() func(chunk []byte) ([]byte, error)

	// NewFromOpenAIStream returns a stateful function that converts a
	// single SSE chunk from OpenAI's stream shape into this adapter's
	// stream shape. Returns nil when no transform is needed (identity).
	NewFromOpenAIStream() func(chunk []byte) ([]byte, error)
}

// Identity is the no-op Translator used by the OpenAI adapter (which is
// the canonical shape, so its wire is already OpenAI). Embed or compose
// to satisfy the interface when no conversion is needed.
type Identity struct{}

func (Identity) ParseRequest(body []byte) (*openai.FullChatRequest, error) {
	r := &openai.FullChatRequest{}
	if err := unmarshal(body, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (Identity) SerializeRequest(r *openai.FullChatRequest) ([]byte, error) {
	return marshal(r)
}

func (Identity) ParseResponse(body []byte) (*openai.ChatResponse, error) {
	r := &openai.ChatResponse{}
	if err := unmarshal(body, r); err != nil {
		return nil, err
	}
	return r, nil
}

func (Identity) SerializeResponse(r *openai.ChatResponse) ([]byte, error) {
	return marshal(r)
}

func (Identity) NewToOpenAIStream() func([]byte) ([]byte, error)   { return nil }
func (Identity) NewFromOpenAIStream() func([]byte) ([]byte, error) { return nil }
