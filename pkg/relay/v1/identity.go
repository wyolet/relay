package v1

import (
	"encoding/json"
	"errors"
)

// IdentityTranslator implements Translator for the canonical shape itself —
// the case the Translator doc anticipates with "when the wire IS canonical."
//
// It exists so canonical can be served as an inbound HTTP shape: the generic
// cross-shape dispatch (inbound → canonical → upstream) works unchanged when
// the inbound translator is this identity. Request parsing is v1.Parse,
// response serialization is a plain marshal, and stream frames pass through
// untouched (the upstream translator already produces canonical SSE).
//
// It is inbound-only: canonical is never an upstream target, so the
// upstream-side methods (SerializeRequest, ParseResponse) are not part of any
// live path and return an error rather than pretend to round-trip.
type IdentityTranslator struct{}

var _ Translator = IdentityTranslator{}

// ParseRequest decodes a canonical request body — the wire is already canonical.
func (IdentityTranslator) ParseRequest(body []byte) (*Request, error) { return Parse(body) }

// SerializeResponse marshals the canonical response as-is. Item MarshalJSON
// methods render the output union; req is unused (no request-echo needed).
func (IdentityTranslator) SerializeResponse(resp *Response, _ *Request) ([]byte, error) {
	return json.Marshal(resp)
}

// NewToCanonicalStream is identity: a canonical upstream stream needs no
// conversion. Canonical is not an upstream today, so this is unused; nil = no-op.
func (IdentityTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) { return nil }

// NewFromCanonicalStream is identity: canonical SSE frames are already in the
// target shape, so dispatch passes them straight through. nil = no-op.
func (IdentityTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) { return nil }

var errCanonicalInboundOnly = errors.New("canonical identity translator is inbound-only: canonical is never an upstream target")

// SerializeRequest is unused — canonical is never serialized to an upstream.
func (IdentityTranslator) SerializeRequest(*Request) ([]byte, error) {
	return nil, errCanonicalInboundOnly
}

// ParseResponse is unused — relay never parses a canonical upstream response.
func (IdentityTranslator) ParseResponse([]byte) (*Response, error) {
	return nil, errCanonicalInboundOnly
}
