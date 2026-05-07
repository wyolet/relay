package anthropic

// Reference for the real transformer: /tmp/litellm/litellm/llms/anthropic/chat/transformation.py
// (AnthropicConfig.transform_request / transform_response)

import (
	"fmt"

	"github.com/wyolet/relay/internal/api/openai"
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
