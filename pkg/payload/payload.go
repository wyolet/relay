// Package payload is the storage contract for request/response body
// capture: the Record shape and the Sink/Reader interfaces its backends
// satisfy. The drivers live in pkg/payload/<backend>, and the lifecycle
// observer that produces Records lives in app/payloadlog (which re-exports
// these types).
//
// A Record is body-only: the request/response bytes keyed by RequestID, and
// nothing else. All per-request metadata (identity, status, routing, tokens,
// timing) lives on the log event (usage.Event) and is read via the log API —
// the body store carries no duplicate of it. The body joins its log record
// by RequestID. The only non-body fields are the timestamp (for partitioning
// / TTL / keying) and the truncation flags.
//
// Zero app/ or internal/ imports — this package is part of the vendorable
// surface.
package payload

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Reader.Get when no captured Record exists for
// the requested id (the request ran without payload logging opted in, or the
// capture aged out). The HTTP layer maps it to 404 / "(not logged)".
var ErrNotFound = errors.New("payload: record not found")

// Record is one captured request/response pair, keyed by RequestID. Bodies
// are stored raw; []byte marshals to base64 in JSON so file/object backends
// are lossless for non-UTF8 content. No metadata beyond the join key, the
// timestamp, and the truncation flags — that all lives on the log event.
type Record struct {
	RequestID         string    `json:"request_id"`
	Timestamp         time.Time `json:"ts"` // partitioning / TTL / keying only
	RequestBody       []byte    `json:"request_body,omitempty"`
	ResponseBody      []byte    `json:"response_body,omitempty"`
	RequestTruncated  bool      `json:"request_truncated,omitempty"`
	ResponseTruncated bool      `json:"response_truncated,omitempty"`
}

// Sink consumes captured Records. Implementations must be non-blocking from
// the caller's perspective — slow I/O belongs inside the sink's own
// buffering. Backends live in pkg/payload/<backend>.
type Sink interface {
	Write(r Record) error
}

// Closer is the optional shutdown contract for a Sink. The emitter
// type-asserts each sink for it and calls Close after the queue drains.
type Closer interface {
	Close() error
}

// Reader is the read-side counterpart to Sink. Body lookup is by request id
// only — there is no List/filter here, because the Logs list is driven by
// the log (usage) store, which holds all the metadata. The body is fetched
// on demand for the detail view.
type Reader interface {
	// Get returns the captured Record for a single request id. Returns
	// ErrNotFound when no capture exists for that id.
	Get(ctx context.Context, requestID string) (Record, error)
}
