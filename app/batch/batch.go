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

import (
	"context"
	"time"
)

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
	Attribution
}

// Attribution is the principal and tenancy that authorised a submission —
// fixed at submit, not whatever the credential resolves to when an item
// finally runs. Ids only; slugs come from the snapshot at emit, as the policy
// name already does. It rides each item's job metadata so execution needs no
// extra read.
type Attribution struct {
	ProjectID      string
	TeamID         string
	PrincipalKind  string
	PrincipalID    string
	CredentialKind string
	CredentialID   string
}

// Caller is the identity a batch request arrives with, resolved by the
// transport. app/batch never reads an HTTP context itself: the inference
// layer owns bearer classification and the Key → ServiceAccount →
// PolicyBinding resolution order, and hands the result down.
type Caller struct {
	Attribution
	// KeyHash is the presented key's hash; empty for a token.
	KeyHash string
	// PolicyID is the already-resolved policy, not the key's raw field.
	PolicyID string
}

// CallerFunc resolves the caller from a request context.
type CallerFunc func(ctx context.Context) *Caller

// Owner is the opaque string a batch is authorized by on read and cancel:
// the bearer's key hash when it has one, else the principal, so a
// token-authenticated caller reaches its own batches and no one else's.
func (c *Caller) Owner() string {
	if c == nil {
		return ""
	}
	if c.KeyHash != "" {
		return c.KeyHash
	}
	return c.PrincipalKind + ":" + c.PrincipalID
}

// Item maps one ordinal within a batch to the jobq job that runs it.
type Item struct {
	BatchID string
	Idx     int
	JobID   string
}
