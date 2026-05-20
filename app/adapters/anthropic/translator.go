package anthropic

import (
	"encoding/json"
	"fmt"

	pkganthropic "github.com/wyolet/relay/pkg/adapters/anthropic"
	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
)

// Translator converts the Anthropic Messages wire shape ↔ OpenAI Chat
// Completions shape. Wraps the pure transforms in pkg/adapters/anthropic.
type Translator struct{}

// ParseRequest unmarshals an Anthropic /v1/messages body and translates
// it to its OpenAI equivalent.
func (Translator) ParseRequest(body []byte) (*pkgopenai.FullChatRequest, error) {
	req, err := pkganthropic.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic.ParseRequest: %w", err)
	}
	return pkganthropic.ToOpenAI(req)
}

// SerializeRequest converts an OpenAI request into an Anthropic
// MessagesRequest and marshals to wire bytes.
func (Translator) SerializeRequest(req *pkgopenai.FullChatRequest) ([]byte, error) {
	out, err := pkganthropic.FromOpenAI(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic.SerializeRequest: %w", err)
	}
	return json.Marshal(out)
}

// ParseResponse unmarshals an Anthropic MessagesResponse and translates
// it to its OpenAI ChatResponse equivalent.
func (Translator) ParseResponse(body []byte) (*pkgopenai.ChatResponse, error) {
	var resp pkganthropic.MessagesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("anthropic.ParseResponse: %w", err)
	}
	return pkganthropic.ToOpenAIResponse(&resp)
}

// SerializeResponse converts an OpenAI ChatResponse into an Anthropic
// MessagesResponse and marshals to wire bytes.
func (Translator) SerializeResponse(resp *pkgopenai.ChatResponse) ([]byte, error) {
	out, err := pkganthropic.FromOpenAIResponse(resp)
	if err != nil {
		return nil, fmt.Errorf("anthropic.SerializeResponse: %w", err)
	}
	return json.Marshal(out)
}

// NewToOpenAIStream returns a per-chunk transformer that converts
// Anthropic SSE events to OpenAI SSE chunks. Stateful across the stream.
func (Translator) NewToOpenAIStream() func(chunk []byte) ([]byte, error) {
	t := &pkganthropic.AnthropicToOpenAI{}
	return t.TransformChunk
}

// NewFromOpenAIStream returns a per-chunk transformer that converts
// OpenAI SSE chunks to Anthropic SSE events. Stateful across the stream.
func (Translator) NewFromOpenAIStream() func(chunk []byte) ([]byte, error) {
	t := &pkganthropic.OpenAIToAnthropic{}
	return t.TransformChunk
}
