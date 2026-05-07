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
type Kind[T any] struct {
	// Name is the human-readable resource kind (e.g. "Provider"). Used in audit logs and errors.
	Name string

	// Decode decodes and validates the request body. Returns 400-ready errors.
	Decode func(r *http.Request) (T, error)

	// List returns all resources from the in-memory snapshot. No PG hit.
	List func(ctx context.Context) ([]T, error)

	// Get returns a single resource by name from the in-memory snapshot.
	// Returns ErrNotFound if missing.
	Get func(ctx context.Context, name string) (T, error)

	// Insert persists a new resource. Called inside a managed transaction.
	Insert func(ctx context.Context, v T) error

	// Update persists changes to an existing resource. Called inside a managed transaction.
	Update func(ctx context.Context, name string, v T) error

	// Delete removes a resource. Called inside a managed transaction.
	Delete func(ctx context.Context, name string) error

	// ResourceID extracts the resource name from a value (e.g. v.Metadata.Name).
	ResourceID func(T) string

	// Patch builds the catalog.Patch for Create/Update validation.
	// If nil, no pre-commit validation is performed.
	Patch func(v T) catalog.Patch

	// PatchDelete builds the catalog.Patch for Delete validation.
	// If nil, no pre-commit validation is performed.
	PatchDelete func(name string) catalog.Patch

	// Summarize produces a diff string for audit logs. Optional; nil => empty.
	Summarize func(before, after T) string
}
