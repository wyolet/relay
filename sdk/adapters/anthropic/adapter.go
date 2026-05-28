package anthropic

// Reference for the real transformer: /tmp/litellm/litellm/llms/anthropic/chat/transformation.py
// (AnthropicConfig.transform_request / transform_response)

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/sdk/adapters/openai"
)

// AnthropicAdapter implements api.Adapter for the Anthropic Messages shape.
type AnthropicAdapter struct{}

func (AnthropicAdapter) Name() string { return "anthropic" }

func (AnthropicAdapter) ParseRequest(body []byte) (any, error) {
	return Parse(body)
}

func (AnthropicAdapter) ToOpenAI(v any) (*openai.FullChatRequest, error) {
	r, ok := v.(*MessagesRequest)
	if !ok {
		return nil, fmt.Errorf("anthropic: ToOpenAI: expected *MessagesRequest, got %T", v)
	}
	return ToOpenAI(r)
}

func (AnthropicAdapter) FromOpenAI(r *openai.FullChatRequest) (any, error) {
	return FromOpenAI(r)
}

func (AnthropicAdapter) ToOpenAIResponse(v any) (*openai.ChatResponse, error) {
	r, ok := v.(*MessagesResponse)
	if !ok {
		return nil, fmt.Errorf("anthropic: ToOpenAIResponse: expected *MessagesResponse, got %T", v)
	}
	return ToOpenAIResponse(r)
}

func (AnthropicAdapter) FromOpenAIResponse(r *openai.ChatResponse) (any, error) {
	return FromOpenAIResponse(r)
}

func (AnthropicAdapter) ParseResponse(body []byte) (any, error) {
	var r MessagesResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("anthropic: ParseResponse: %w", err)
	}
	return &r, nil
}

// Shim wraps AnthropicAdapter so it satisfies pipeline.TransformAdapter
// (which uses any return types to avoid import cycles).
type Shim struct{ AnthropicAdapter }

func (s Shim) ToOpenAI(req any) (any, error) { return s.AnthropicAdapter.ToOpenAI(req) }
func (s Shim) FromOpenAI(req any) (any, error) {
	r, ok := req.(*openai.FullChatRequest)
	if !ok {
		return nil, fmt.Errorf("anthropic shim: FromOpenAI: expected *openai.FullChatRequest, got %T", req)
	}
	return s.AnthropicAdapter.FromOpenAI(r)
}
func (s Shim) ToOpenAIResponse(resp any) (any, error) {
	return s.AnthropicAdapter.ToOpenAIResponse(resp)
}
func (s Shim) FromOpenAIResponse(resp any) (any, error) {
	r, ok := resp.(*openai.ChatResponse)
	if !ok {
		return nil, fmt.Errorf("anthropic shim: FromOpenAIResponse: expected *openai.ChatResponse, got %T", resp)
	}
	return s.AnthropicAdapter.FromOpenAIResponse(r)
}
