// Package crud provides a generic HTTP handler factory for admin CRUD endpoints.
// Each Kind[T] produces five http.HandlerFuncs (List, Get, Create, Update, Delete)
// given per-kind decode/persist callbacks. Auth is assumed to have been checked upstream.
package crud

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/reqid"
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

// Handlers returns the five HTTP handler funcs for the resource kind.
func (k *Kind[T]) Handlers(deps Deps) (list, get, create, update, del http.HandlerFunc) {
	list = k.listHandler(deps)
	get = k.getHandler(deps)
	create = k.createHandler(deps)
	update = k.updateHandler(deps)
	del = k.deleteHandler(deps)
	return
}

// --- read handlers ---

func (k *Kind[T]) listHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := k.List(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}
		if items == nil {
			items = []T{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func (k *Kind[T]) getHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := resourceName(r)
		v, err := k.Get(r.Context(), name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
					fmt.Sprintf("%s %q not found", k.Name, name))
				return
			}
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// --- write handlers ---

func (k *Kind[T]) createHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		v, err := k.Decode(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
			return
		}

		if k.Patch != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.Patch(v)); verr != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed", verr.Error())
				return
			}
		}

		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return k.Insert(ctx, v)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after create; snapshot may be stale",
				"kind", k.Name, "name", k.ResourceID(v), "err", err)
			writeErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		name := k.ResourceID(v)
		emitAudit(deps.Logger, ctx, r, k.Name, name, "create", "")

		created, err := k.Get(ctx, name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error",
				"created but could not read back: "+err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, created)
	}
}

func (k *Kind[T]) updateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := resourceName(r)

		before, err := k.Get(ctx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeErr(w, http.StatusNotFound, "invalid_request_error", "not_found",
					fmt.Sprintf("%s %q not found", k.Name, name))
				return
			}
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		v, err := k.Decode(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request_error", "invalid_body", err.Error())
			return
		}

		if k.Patch != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.Patch(v)); verr != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed", verr.Error())
				return
			}
		}

		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return k.Update(ctx, name, v)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after update; snapshot may be stale",
				"kind", k.Name, "name", name, "err", err)
			writeErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		diff := ""
		if k.Summarize != nil {
			diff = k.Summarize(before, v)
		}
		emitAudit(deps.Logger, ctx, r, k.Name, name, "update", diff)

		updated, err := k.Get(ctx, name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error",
				"updated but could not read back: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, updated)
	}
}

func (k *Kind[T]) deleteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		name := resourceName(r)

		if k.PatchDelete != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.PatchDelete(name)); verr != nil {
				writeErr(w, http.StatusBadRequest, "invalid_request_error", "validation_failed", verr.Error())
				return
			}
		}

		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return k.Delete(ctx, name)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "server_error", "internal_error", err.Error())
			return
		}

		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after delete; snapshot may be stale",
				"kind", k.Name, "name", name, "err", err)
			writeErr(w, http.StatusInternalServerError, "server_error", "reload_failed",
				"mutation committed but reload failed: "+err.Error())
			return
		}

		emitAudit(deps.Logger, ctx, r, k.Name, name, "delete", "")
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- helpers ---

// resourceName extracts the resource name from the URL path (last non-empty segment).
func resourceName(r *http.Request) string {
	path := strings.TrimRight(r.URL.Path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

func emitAudit(log *slog.Logger, ctx context.Context, r *http.Request, kind, name, action, diff string) {
	tok := r.Header.Get("X-Relay-Admin-Token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	sum := sha256.Sum256([]byte(tok))
	hash := fmt.Sprintf("%x", sum[:6]) // 12 hex chars

	log.InfoContext(ctx, "admin: "+strings.ToLower(kind)+" "+action,
		"kind", kind,
		"name", name,
		"action", action,
		"token_hash", hash,
		"request_id", reqid.From(ctx),
		"diff", diff,
	)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"type":    errType,
			"code":    code,
			"message": message,
		},
	})
}
