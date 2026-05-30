// Package payload is the storage contract for request/response body
// capture: the Record shape and the Sink interface its backends satisfy.
// It mirrors pkg/usage — the vendorable contract lives here, the drivers
// in pkg/payload/<backend>, and the lifecycle observer that produces
// Records lives in app/payloadlog (which re-exports these types).
//
// Zero app/ or internal/ imports — this package is part of the
// vendorable surface.
package payload

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Reader.Get when no captured Record exists for
// the requested id. The HTTP layer maps it to 404.
var ErrNotFound = errors.New("payload: record not found")

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

// Reader is the read-side counterpart to Sink, serving the Logs view.
// Implementations satisfy this against whatever store the Sink wrote to —
// JSONL file or object store today. Consumers depend on the interface, not
// the backend.
type Reader interface {
	// List returns Records matching q in reverse-chronological order
	// (newest first), capped at q.Limit. The bodies are stripped (nil) —
	// List is the metadata view that drives the Logs table; fetch the full
	// request/response via Get. The truncation flags are preserved so the
	// UI can flag a clipped capture.
	List(ctx context.Context, q Query) ([]Record, error)

	// Get returns the full Record — bodies included — for a single request
	// id. Returns ErrNotFound when no capture exists for that id (the
	// request may have run without payload logging opted in).
	Get(ctx context.Context, requestID string) (Record, error)
}

// Query filters the captured Record stream. All fields are optional; zero
// values mean "no filter on this dimension." It mirrors usage.EventQuery
// minus the dimensions a Record doesn't carry (tokens, finish_reason).
type Query struct {
	// Time bounds. Lower bound: From if set, else now()-Since, else
	// unbounded. Upper bound: To if set, else unbounded. From/To are
	// absolute and take precedence over the relative Since.
	Since time.Duration
	From  time.Time
	To    time.Time

	// Categorical filters — match any value in the slice (set membership).
	// Empty slice means no filter on that dimension.
	RelayKeyHash []string
	PolicyID     []string
	ModelID      []string
	HostID       []string
	Source       []string // "pipeline" | "proxy" | "ws" | "batch"
	ErrorKind    []string

	// StatusMin / StatusMax restrict by HTTP status. Zero values mean
	// unbounded on that side.
	StatusMin int
	StatusMax int

	// Limit caps the number of records returned. <=0 → DefaultLimit.
	Limit int

	// CursorTS / CursorID implement keyset pagination: when CursorTS is
	// non-zero, only records strictly older than the cursor under the
	// (ts DESC, request_id DESC) ordering are returned.
	CursorTS time.Time
	CursorID string
}

// DefaultLimit caps an unbounded List so a misconfigured caller doesn't
// pull the whole store at once.
const DefaultLimit = 100

// MaxLimit clamps Limit to avoid OOM on a hostile request.
const MaxLimit = 10_000
