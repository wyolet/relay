// Package api defines the cross-shape adapter interface.
// OpenAI shape is the canonical hub; all other shapes convert through it.
package adapters

import (
	"errors"

	"github.com/wyolet/relay/sdk/adapters/openai"
)

// ErrNotImplemented is returned by stub adapter methods not yet written.
var ErrNotImplemented = errors.New("not implemented")

// Adapter converts between a shape-native request/response and the OpenAI hub
// format. Each shape package (openai, anthropic, …) exports a value that
// implements this interface.
type Adapter interface {
	// Name returns the canonical shape name, e.g. "openai" or "anthropic".
	Name() string

	// ParseRequest decodes raw bytes into the shape-native request struct.
	ParseRequest(body []byte) (any, error)

	// ToOpenAI converts a shape-native request to the OpenAI hub format.
	ToOpenAI(req any) (*openai.FullChatRequest, error)

	// FromOpenAI converts an OpenAI hub request to the shape-native format.
	FromOpenAI(req *openai.FullChatRequest) (any, error)

	// ToOpenAIResponse converts a shape-native response to the OpenAI hub format.
	ToOpenAIResponse(resp any) (*openai.ChatResponse, error)

	// FromOpenAIResponse converts an OpenAI hub response to the shape-native format.
	FromOpenAIResponse(resp *openai.ChatResponse) (any, error)
}
