package openai

import (
	"encoding/json"
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

func (OpenAIAdapter) ParseResponse(body []byte) (any, error) {
	var r ChatResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("openai adapter: ParseResponse: %w", err)
	}
	return &r, nil
}

// Shim wraps OpenAIAdapter so it satisfies pipeline.TransformAdapter
// (which uses any return types to avoid import cycles).
type Shim struct{ OpenAIAdapter }

func (s Shim) ToOpenAI(req any) (any, error)           { return s.OpenAIAdapter.ToOpenAI(req) }
func (s Shim) FromOpenAI(req any) (any, error) {
	r, ok := req.(*FullChatRequest)
	if !ok {
		return nil, fmt.Errorf("openai shim: FromOpenAI: expected *FullChatRequest, got %T", req)
	}
	return s.OpenAIAdapter.FromOpenAI(r)
}
func (s Shim) ToOpenAIResponse(resp any) (any, error)  { return s.OpenAIAdapter.ToOpenAIResponse(resp) }
func (s Shim) FromOpenAIResponse(resp any) (any, error) {
	r, ok := resp.(*ChatResponse)
	if !ok {
		return nil, fmt.Errorf("openai shim: FromOpenAIResponse: expected *ChatResponse, got %T", resp)
	}
	return s.OpenAIAdapter.FromOpenAIResponse(r)
}
