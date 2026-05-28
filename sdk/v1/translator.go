package v1

// Translator converts between a vendor's wire shape and canonical.
// All methods are stateless — a single Translator value is reused across
// requests. Per-stream state lives in the closures returned by the two
// stream factories.
type Translator interface {
	// ParseRequest reads a vendor's wire request body and returns canonical.
	ParseRequest(body []byte) (*Request, error)

	// SerializeRequest emits a vendor's wire request body from canonical.
	SerializeRequest(req *Request) ([]byte, error)

	// ParseResponse reads a vendor's wire response body and returns canonical.
	ParseResponse(body []byte) (*Response, error)

	// SerializeResponse emits a vendor's wire response body from canonical.
	// req is the original canonical request, passed for wire shapes that
	// require request-echo on the response (e.g. OpenAI Responses). May
	// be nil if the caller doesn't have it or the adapter doesn't need it.
	SerializeResponse(resp *Response, req *Request) ([]byte, error)

	// NewToCanonicalStream returns a stateful per-stream function that
	// converts one SSE chunk from the vendor's stream shape into one or
	// more canonical SSE chunks. Returns nil if no transform is needed
	// (identity, e.g. when the wire IS canonical).
	NewToCanonicalStream() func(chunk []byte) ([]byte, error)

	// NewFromCanonicalStream returns a stateful per-stream function that
	// converts one SSE chunk from canonical into one or more chunks of
	// the vendor's stream shape. Returns nil for identity.
	NewFromCanonicalStream() func(chunk []byte) ([]byte, error)
}
