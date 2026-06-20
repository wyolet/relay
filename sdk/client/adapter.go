package client

import (
	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Adapter is the atomic wire bundle: translator, path resolution, and auth
// always travel together. Selecting an adapter swaps all three at once.
type Adapter struct {
	translator  v1.Translator
	path        string
	pathFn      func(model string, stream bool) string
	auth        Auth
	defaultOpts []Option
}

var adapters = map[string]Adapter{
	"openai": {
		translator: openai.CCTranslator{},
		path:       "/v1/chat/completions",
		auth:       Auth{Header: "Authorization", Scheme: "Bearer"},
	},
	// openai_responses speaks the OpenAI Responses API (/responses) — the wire
	// the Codex/ChatGPT subscription backend uses. Name matches the catalog
	// binding adapter so For() resolves it too. The translator forces
	// store:false (no server-side persistence).
	"openai_responses": {
		translator: openai.ResponsesTranslator{},
		path:       "/responses",
		auth:       Auth{Header: "Authorization", Scheme: "Bearer"},
	},
	"anthropic": {
		translator: anthropic.AnthropicTranslator{},
		path:       "/v1/messages",
		auth:       Auth{Header: "x-api-key"},
		defaultOpts: []Option{
			WithHeader("anthropic-version", "2023-06-01"),
		},
	},
	"gemini": {
		translator: gemini.GeminiTranslator{},
		pathFn:     geminiPath,
		auth:       Auth{Header: "x-goog-api-key"},
	},
}

// geminiPath mirrors relay's server-side Gemini spec: the model and the
// sync/stream choice live in the URL path, not the body.
func geminiPath(model string, stream bool) string {
	if stream {
		return "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
	}
	return "/v1beta/models/" + model + ":generateContent"
}

// AdapterByName returns a registered adapter bundle, or false.
func AdapterByName(name string) (Adapter, bool) {
	a, ok := adapters[name]
	return a, ok
}

// KnownAdapterNames returns every adapter name registered in the SDK.
func KnownAdapterNames() []string {
	out := make([]string, 0, len(adapters))
	for name := range adapters {
		out = append(out, name)
	}
	return out
}

func (a Adapter) apply(c *Client) {
	c.translator = a.translator
	c.path = a.path
	c.pathFn = a.pathFn
	c.auth = a.auth
	for _, o := range a.defaultOpts {
		o(c)
	}
}
