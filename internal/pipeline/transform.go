package pipeline

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/wyolet/relay/pkg/transport"
)

// streamTransformerFactories maps (inbound, upstream) shape-name pairs to a
// factory that produces a stateful per-chunk SSE transform function. Packages
// register themselves at init time via RegisterStreamTransformerFactory.
var streamTransformerFactories = map[[2]string]func() func([]byte) ([]byte, error){}

// RegisterStreamTransformerFactory registers a factory for the given
// (inbound, upstream) shape pair. Call from init() in the shape package.
func RegisterStreamTransformerFactory(inbound, upstream string, factory func() func([]byte) ([]byte, error)) {
	streamTransformerFactories[[2]string{inbound, upstream}] = factory
}

// ErrCrossShapeStreamingNotSupported is returned when inbound and upstream
// shapes differ and the request is streaming. Streaming cross-shape transform
// is deferred to a follow-up PR; callers should surface this as a 501.
var ErrCrossShapeStreamingNotSupported = errors.New("cross-shape streaming transform not supported")

// TransformAdapter is the narrow interface pipeline needs from shape adapters.
// It deliberately uses any for hub types to break the import cycle with
// internal/api (which imports internal/api/openai, which imports internal/pipeline).
// Concrete adapters (openai.OpenAIAdapter, anthropic.AnthropicAdapter) satisfy
// this interface; callers pass them as TransformAdapter values.
type TransformAdapter interface {
	// Name returns the canonical shape name, e.g. "openai" or "anthropic".
	Name() string
	// ParseRequest decodes raw bytes into the shape-native request struct.
	ParseRequest(body []byte) (any, error)
	// ToOpenAI converts a shape-native request to the OpenAI hub format (any wraps *openai.FullChatRequest).
	ToOpenAI(req any) (any, error)
	// FromOpenAI converts an OpenAI hub request (any wraps *openai.FullChatRequest) to the shape-native format.
	FromOpenAI(req any) (any, error)
	// ToOpenAIResponse converts a shape-native response to the OpenAI hub format (any wraps *openai.ChatResponse).
	ToOpenAIResponse(resp any) (any, error)
	// FromOpenAIResponse converts an OpenAI hub response (any wraps *openai.ChatResponse) to the shape-native format.
	FromOpenAIResponse(resp any) (any, error)
	// ParseResponse decodes a raw upstream response body into the shape-native response struct.
	ParseResponse(body []byte) (any, error)
}

// TransformResult bundles the (possibly transformed) upstream request body and
// a finisher that translates the upstream response back to the inbound shape.
// When shapes match, Body is the original slice (zero-copy) and Finisher is nil.
type TransformResult struct {
	Body     []byte
	Finisher func(msg *transport.Message) (*transport.Message, error)
}

// ApplyTransform decides whether a shape transform is needed.
//
//   - Same shape → zero-copy passthrough; Finisher is nil.
//   - Different shape + streaming → ErrCrossShapeStreamingNotSupported.
//   - Different shape + non-streaming → transforms via OpenAI canonical hub;
//     Finisher converts the single upstream response back to the inbound shape.
//
// Either adapter may be nil; if so, passthrough is used.
func ApplyTransform(inbound, upstream TransformAdapter, body []byte) (TransformResult, error) {
	if inbound == nil || upstream == nil || inbound.Name() == upstream.Name() {
		return TransformResult{Body: body}, nil
	}

	if isStreaming(body) {
		factory, ok := streamTransformerFactories[[2]string{inbound.Name(), upstream.Name()}]
		if !ok {
			return TransformResult{}, ErrCrossShapeStreamingNotSupported
		}
		// Transform the request body (inbound→upstream shape) for upstream forwarding.
		inboundReq, err := inbound.ParseRequest(body)
		if err != nil {
			return TransformResult{}, err
		}
		hub, err := inbound.ToOpenAI(inboundReq)
		if err != nil {
			return TransformResult{}, err
		}
		upstreamReq, err := upstream.FromOpenAI(hub)
		if err != nil {
			return TransformResult{}, err
		}
		upstreamBody, err := json.Marshal(upstreamReq)
		if err != nil {
			return TransformResult{}, err
		}

		chunkFn := factory()
		finisher := func(msg *transport.Message) (*transport.Message, error) {
			if len(msg.Body) == 0 {
				return msg, nil
			}
			out, err := chunkFn(msg.Body)
			if err != nil {
				return nil, err
			}
			if out == nil {
				// No-op chunk (ping, etc.) — return empty body so pipeline skips emission.
				return &transport.Message{Headers: msg.Headers}, nil
			}
			result := &transport.Message{
				Headers: make(map[string]string, len(msg.Headers)),
				Body:    out,
			}
			for k, v := range msg.Headers {
				result.Headers[k] = v
			}
			return result, nil
		}
		return TransformResult{Body: upstreamBody, Finisher: finisher}, nil
	}

	// Parse inbound → hub → upstream native.
	inboundReq, err := inbound.ParseRequest(body)
	if err != nil {
		return TransformResult{}, err
	}
	hub, err := inbound.ToOpenAI(inboundReq)
	if err != nil {
		return TransformResult{}, err
	}
	upstreamReq, err := upstream.FromOpenAI(hub)
	if err != nil {
		return TransformResult{}, err
	}
	upstreamBody, err := json.Marshal(upstreamReq)
	if err != nil {
		return TransformResult{}, err
	}

	ia := inbound
	ua := upstream

	finisher := func(msg *transport.Message) (*transport.Message, error) {
		if len(msg.Body) == 0 {
			return msg, nil
		}
		upstreamResp, err := ua.ParseResponse(msg.Body)
		if err != nil {
			return nil, err
		}
		hubResp, err := ua.ToOpenAIResponse(upstreamResp)
		if err != nil {
			return nil, err
		}
		inboundResp, err := ia.FromOpenAIResponse(hubResp)
		if err != nil {
			return nil, err
		}
		outBody, err := json.Marshal(inboundResp)
		if err != nil {
			return nil, err
		}
		out := &transport.Message{
			Headers: make(map[string]string, len(msg.Headers)),
			Body:    outBody,
		}
		for k, v := range msg.Headers {
			out.Headers[k] = v
		}
		return out, nil
	}

	return TransformResult{Body: upstreamBody, Finisher: finisher}, nil
}

// isStreaming reports whether body contains "stream":true (with optional space).
func isStreaming(body []byte) bool {
	return bytes.Contains(body, []byte(`"stream":true`)) ||
		bytes.Contains(body, []byte(`"stream": true`))
}
