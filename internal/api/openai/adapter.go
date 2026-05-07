package openai

import (
	"fmt"
)

// OpenAIAdapter implements api.Adapter for the OpenAI shape.
// It is the identity adapter: the hub format IS the OpenAI format.
type OpenAIAdapter struct{}

func (OpenAIAdapter) Name() string { return "openai" }

func (OpenAIAdapter) ParseRequest(body []byte) (any, error) {
	return Parse(nil, body, nil)
}

func (OpenAIAdapter) ToOpenAI(req any) (*FullChatRequest, error) {
	cr, ok := req.(*ChatRequest)
	if !ok {
		return nil, fmt.Errorf("openai adapter: expected *ChatRequest, got %T", req)
	}
	// ChatRequest is the lightweight hot-path struct; return a FullChatRequest
	// populated from the fields we extracted during parsing.
	return &FullChatRequest{
		Model: cr.Model,
	}, nil
}

func (OpenAIAdapter) FromOpenAI(req *FullChatRequest) (any, error) {
	return req, nil
}

func (OpenAIAdapter) ToOpenAIResponse(resp any) (*ChatResponse, error) {
	cr, ok := resp.(*ChatResponse)
	if !ok {
		return nil, fmt.Errorf("openai adapter: expected *ChatResponse, got %T", resp)
	}
	return cr, nil
}

func (OpenAIAdapter) FromOpenAIResponse(resp *ChatResponse) (any, error) {
	return resp, nil
}
