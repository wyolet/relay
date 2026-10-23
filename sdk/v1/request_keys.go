package v1

// RequestKeys names the top-level members of a wire shape's request body that a router reads from a body it forwards without translating it.
type RequestKeys struct {
	// Stream is the member that selects streaming. Empty when the shape selects it outside the body (the URL).
	Stream string

	// StreamOn is the string value of Stream that selects streaming. Empty when Stream holds a bool.
	StreamOn string
}

// RequestKeysProvider is an optional companion to Translator for wire shapes whose request bodies are read without being translated.
type RequestKeysProvider interface {
	RequestKeys() RequestKeys
}

// RequestKeys names output_mode as the canonical stream selector.
func (IdentityTranslator) RequestKeys() RequestKeys {
	return RequestKeys{Stream: "output_mode", StreamOn: OutputModeStream}
}
