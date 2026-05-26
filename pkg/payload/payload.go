// Package payload is the storage contract for request/response body
// capture: the Record shape and the Sink interface its backends satisfy.
// It mirrors pkg/usage — the vendorable contract lives here, the drivers
// in pkg/payload/<backend>, and the lifecycle observer that produces
// Records lives in app/payloadlog (which re-exports these types).
//
// Zero app/ or internal/ imports — this package is part of the
// vendorable surface.
package payload

import "time"

// Record is one captured request/response pair. Bodies are stored raw;
// []byte marshals to base64 in JSON so file/object backends are lossless
// for non-UTF8 content. Identity fields mirror usage.Event so a Record
// joins to its usage row by RequestID.
type Record struct {
	RequestID    string    `json:"request_id"`
	Timestamp    time.Time `json:"ts"`
	Source       string    `json:"source"`
	Status       int       `json:"status"`
	Streamed     bool      `json:"streamed,omitempty"`
	RelayKeyHash string    `json:"relay_key_hash,omitempty"`
	PolicyID     string    `json:"policy_id,omitempty"`
	ModelID      string    `json:"model_id,omitempty"`
	HostID       string    `json:"host_id,omitempty"`
	ErrorKind    string    `json:"error_kind,omitempty"`

	RequestBody       []byte `json:"request_body,omitempty"`
	ResponseBody      []byte `json:"response_body,omitempty"`
	RequestTruncated  bool   `json:"request_truncated,omitempty"`
	ResponseTruncated bool   `json:"response_truncated,omitempty"`
}

// Sink consumes captured Records. Implementations must be non-blocking
// from the caller's perspective — slow I/O belongs inside the sink's own
// buffering. Backends live in pkg/payload/<backend>.
type Sink interface {
	Write(r Record) error
}

// Closer is the optional shutdown contract for a Sink. The emitter
// type-asserts each sink for it and calls Close after the queue drains.
type Closer interface {
	Close() error
}
