// Package batch is the batch subsystem: it accepts bulk inference submissions,
// runs each item as a background job (via the jobq module), and exposes
// submit/poll/cancel/results over the customer-facing /v1 surface.
//
// Layering: batch is a CONSUMER of jobq. jobq owns durable execution (claim,
// retry, crash recovery) and the per-item payload bytes; this package owns the
// batch concept — the batch record, the batch→job mapping, the inference
// handler that turns one item into an upstream call (reusing the realtime
// pipeline), and the HTTP API. The relay hot path is untouched; admission only
// validates and enqueues, execution happens off to the side.
package batch

import "time"

// Status is the coarse, cached lifecycle of a batch. The authoritative per-item
// state lives in jobq; this is a cheap roll-up plus the terminal cancellation
// marker.
type Status string

const (
	StatusQueued    Status = "queued"    // accepted, items enqueued
	StatusRunning   Status = "running"   // at least one item has started
	StatusCompleted Status = "completed" // all items finished successfully
	StatusFailed    Status = "failed"    // all items finished, some failed
	StatusCancelled Status = "cancelled" // caller cancelled
)

// Batch is the durable record of one bulk submission.
type Batch struct {
	ID           string
	RelayKeyHash string // owner — used for authz on read/cancel
	PolicyID     string
	InboundShape string // the wire shape items are expressed in (adapter spec name)
	Status       Status
	TotalItems   int
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

// Item maps one ordinal within a batch to the jobq job that runs it.
type Item struct {
	BatchID string
	Idx     int
	JobID   string
}
