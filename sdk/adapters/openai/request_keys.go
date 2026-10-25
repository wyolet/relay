package openai

import v1 "github.com/wyolet/relay/sdk/v1"

var (
	_ v1.RequestKeysProvider = CCTranslator{}
	_ v1.RequestKeysProvider = ResponsesTranslator{}
)

// RequestKeys names Chat Completions' stream bool.
func (CCTranslator) RequestKeys() v1.RequestKeys { return v1.RequestKeys{Stream: "stream"} }

// RequestKeys names the Responses API's stream bool.
func (ResponsesTranslator) RequestKeys() v1.RequestKeys { return v1.RequestKeys{Stream: "stream"} }
