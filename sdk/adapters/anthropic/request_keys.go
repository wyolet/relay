package anthropic

import v1 "github.com/wyolet/relay/sdk/v1"

var _ v1.RequestKeysProvider = AnthropicTranslator{}

// RequestKeys names the Messages API's stream bool.
func (AnthropicTranslator) RequestKeys() v1.RequestKeys { return v1.RequestKeys{Stream: "stream"} }
