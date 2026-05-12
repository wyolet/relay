// Package crud provides a generic HTTP handler factory for admin CRUD endpoints.
// Each Kind[T] carries per-kind decode/persist callbacks. Auth is assumed to have been
// checked upstream. The huma-typed operations are registered via RegisterOps (huma.go).
package crud

import (
	"context"
	"errors"
	"net/http"

	"github.com/wyolet/relay/internal/catalog"
	"log/slog"
)

// ErrNotFound is returned by Get callbacks when the resource does not exist.
var ErrNotFound = errors.New("not found")

// Patcher validates a proposed post-write snapshot. Implemented by *catalog.PGStore.
type Patcher interface {
	ValidateWithPatch(patch catalog.Patch) error
}

// Reloader atomically swaps the in-memory snapshot from Postgres.
type Reloader interface {
	Reload(ctx context.Context) error
}

// TxRunner runs fn inside a transaction, committing on nil error and rolling back on error.
// Implemented by the storage adapter in cmd/relay (wrapping *storage.Storage.WithTx).
type TxRunner interface {
	RunInTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Deps holds dependencies shared across all Kind handlers.
type Deps struct {
	Tx       TxRunner
	Patcher  Patcher
	Reloader Reloader
	Logger   *slog.Logger
}

// Kind is a generic admin CRUD factory for resource type T.
//
// Identity model:
//   - Each resource has an immutable id (UUIDv7, server-stamped on create) and a
//     mutable slug (Metadata.Name).
//   - GET routes accept either: /{base}/{slugOrID}.
//   - POST creates and the server stamps id (StampID).
//   - PUT/DELETE are id-routed at /{base}/by-id/{id} so renames don't break URLs.
type Kind[T any] struct {
	// Name is the human-readable resource kind (e.g. "Provider"). Used in audit logs and errors.
	Name string

	// Decode decodes and validates the request body. Returns 400-ready errors.
	Decode func(r *http.Request) (T, error)

	// List returns all resources from the in-memory snapshot. No PG hit.
	List func(ctx context.Context) ([]T, error)

	// GetBySlugOrID returns a single resource. Implementations should try id
	// lookup first when the path component parses as a UUID, then fall back to
	// slug. Returns ErrNotFound if missing.
	GetBySlugOrID func(ctx context.Context, slugOrID string) (T, error)

	// GetByID returns a single resource by id. Used by PUT/DELETE handlers to
	// resolve the current row before mutation. Returns ErrNotFound if missing.
	GetByID func(ctx context.Context, id string) (T, error)

	// StampID is called on POST before validation. The implementation stamps
	// Metadata.ID with a fresh UUIDv7 if empty, and may derive Metadata.Name
	// (slug) from DisplayName with collision suffix. May mutate v.
	StampID func(ctx context.Context, v T) error

	// Insert persists a new resource. Called inside a managed transaction
	// after StampID and Patch validation.
	Insert func(ctx context.Context, v T) error

	// UpdateByID persists changes to an existing resource (id-routed).
	// Called inside a managed transaction.
	UpdateByID func(ctx context.Context, id string, v T) error

	// DeleteByID removes a resource by id. Called inside a managed transaction.
	DeleteByID func(ctx context.Context, id string) error

	// ResourceID extracts the slug for display/audit. Should remain valid even
	// after slug edits — call on the post-write copy.
	ResourceID func(T) string

	// ResourceIDValue extracts the immutable id for response location headers
	// and audit (e.g. v.Metadata.ID).
	ResourceIDValue func(T) string

	// Patch builds the catalog.Patch for Create/Update validation.
	// If nil, no pre-commit validation is performed.
	Patch func(v T) catalog.Patch

	// PatchDelete builds the catalog.Patch for Delete validation. Receives the
	// slug (looked up from id by the handler). If nil, no pre-commit
	// validation is performed.
	PatchDelete func(slug string) catalog.Patch

	// Summarize produces a diff string for audit logs. Optional; nil => empty.
	Summarize func(before, after T) string

	// Guard is called before Update and Delete with the existing resource and
	// the proposed new resource. proposed is nil on delete. It may reject the
	// mutation by returning an error (use huma.NewError for a specific HTTP
	// status, e.g. 403). Optional; nil disables the check.
	Guard func(ctx context.Context, existing, proposed T) error
}
