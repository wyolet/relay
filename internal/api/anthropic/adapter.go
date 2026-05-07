package anthropic

// Reference for the real transformer: /tmp/litellm/litellm/llms/anthropic/chat/transformation.py
// (AnthropicConfig.transform_request / transform_response)

import (
	"github.com/wyolet/relay/internal/api"
	"github.com/wyolet/relay/internal/api/openai"
)

// AnthropicAdapter implements api.Adapter for the Anthropic Messages shape.
type AnthropicAdapter struct{}

func (AnthropicAdapter) Name() string { return "anthropic" }

func (AnthropicAdapter) ParseRequest(body []byte) (any, error) {
	return Parse(body)
}

func (AnthropicAdapter) ToOpenAI(_ any) (*openai.FullChatRequest, error) {
	return nil, api.ErrNotImplemented
}

func (AnthropicAdapter) FromOpenAI(_ *openai.FullChatRequest) (any, error) {
	return nil, api.ErrNotImplemented
}

func (AnthropicAdapter) ToOpenAIResponse(_ any) (*openai.ChatResponse, error) {
	return nil, api.ErrNotImplemented
}

func (AnthropicAdapter) FromOpenAIResponse(_ *openai.ChatResponse) (any, error) {
	return nil, api.ErrNotImplemented
}
