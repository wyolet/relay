package v1

import "encoding/json"

// IdentityTranslator implements Translator for the canonical shape itself —
// the case the Translator doc anticipates with "when the wire IS canonical."
// Because the wire and canonical are the same, every method is a plain
// (de)serialization with no shape conversion, and the stream factories are
// no-ops (nil).
//
// It serves two symmetric roles:
//   - Server inbound: the relay's /v1/generate spec uses it so the generic
//     cross-shape dispatch (inbound → canonical → upstream) serves canonical
//     with no special-casing.
//   - Client target: a canonical client (pkg/relay/client) uses it to talk to
//     a relay server — serialize the canonical request, parse the canonical
//     response — exactly as a vendor translator is used to talk to a vendor.
type IdentityTranslator struct{}

var _ Translator = IdentityTranslator{}

// ParseRequest decodes a canonical request body — the wire is already canonical.
func (IdentityTranslator) ParseRequest(body []byte) (*Request, error) { return Parse(body) }

// SerializeRequest marshals a canonical request to the canonical wire (input
// included, model as string-or-array — see Request.MarshalJSON).
func (IdentityTranslator) SerializeRequest(req *Request) ([]byte, error) {
	return json.Marshal(req)
}

// ParseResponse decodes a canonical response body (reconstructing the Output
// item union — see the package ParseResponse).
func (IdentityTranslator) ParseResponse(body []byte) (*Response, error) {
	return ParseResponse(body)
}

// SerializeResponse marshals the canonical response as-is. Item MarshalJSON
// methods render the output union; req is unused (no request-echo needed).
func (IdentityTranslator) SerializeResponse(resp *Response, _ *Request) ([]byte, error) {
	return json.Marshal(resp)
}

// NewToCanonicalStream is identity: a canonical stream needs no conversion. nil = no-op.
func (IdentityTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) { return nil }

// NewFromCanonicalStream is identity: canonical frames are already in the target
// shape, so they pass straight through. nil = no-op.
func (IdentityTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) { return nil }
