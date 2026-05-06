// Package crud — huma.go
// RegisterOps registers typed huma operations for a Kind[T].
// This gives the generated OpenAPI spec full request/response schemas.
// The existing Handlers() method (used by chi-mounted tests) is preserved.
package crud

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

// ListOutput wraps a slice of T for a typed list response.
type ListOutput[T any] struct {
	Body struct {
		Items []T `json:"items" doc:"All resources of this kind."`
	}
}

// ItemOutput wraps a single T for a typed get/create/update response.
type ItemOutput[T any] struct {
	Body T
}

// NameInput is the path-param input for single-resource operations.
type NameInput struct {
	Name string `path:"name" doc:"Resource name."`
}

// BodyInput is the request body input for create/update operations.
type BodyInput[T any] struct {
	Body T
}

// NameBodyInput combines path param + request body for update.
type NameBodyInput[T any] struct {
	Name string `path:"name" doc:"Resource name."`
	Body T
}

// DeleteInput contains just the path param; output is empty (204).
type DeleteInput struct {
	Name string `path:"name" doc:"Resource name."`
}

// humaError converts a status code + message into a huma error.
// huma.NewError must already be overridden by the caller to produce OpenAI envelopes.
func humaError(status int, msg string) error {
	return huma.NewError(status, msg)
}

// RegisterOps registers the five standard CRUD operations for Kind[T] on the huma API.
// It bypasses the http.HandlerFunc layer and calls domain logic directly, giving huma
// full typed input/output structs for schema generation.
//
//   - GET    {base}        → list
//   - GET    {base}/{name} → get
//   - POST   {base}        → create (body T)
//   - PUT    {base}/{name} → update (body T)
//   - DELETE {base}/{name} → delete (204)
func RegisterOps[T any](
	api huma.API,
	base string,   // e.g. "/admin/providers"
	singular string, // e.g. "provider"
	plural string,   // e.g. "providers"
	k *Kind[T],
	deps Deps,
	middlewares huma.Middlewares,
) {
	nameParam := base + "/{name}"

	// --- List ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_list",
		Method:      http.MethodGet,
		Path:        base,
		Summary:     "List " + plural,
		Tags:        []string{"admin"},
		Errors:      []int{500},
		Middlewares: middlewares,
	}, func(ctx context.Context, _ *struct{}) (*ListOutput[T], error) {
		items, err := k.List(ctx)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if items == nil {
			items = []T{}
		}
		out := &ListOutput[T]{}
		out.Body.Items = items
		return out, nil
	})

	// --- Get ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_get",
		Method:      http.MethodGet,
		Path:        nameParam,
		Summary:     "Get " + singular,
		Tags:        []string{"admin"},
		Errors:      []int{404, 500},
		Middlewares: middlewares,
	}, func(ctx context.Context, in *NameInput) (*ItemOutput[T], error) {
		v, err := k.Get(ctx, in.Name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, humaError(http.StatusNotFound,
					fmt.Sprintf("%s %q not found", k.Name, in.Name))
			}
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		return &ItemOutput[T]{Body: v}, nil
	})

	// --- Create ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_create",
		Method:      http.MethodPost,
		Path:        base,
		Summary:     "Create " + singular,
		Tags:        []string{"admin"},
		Errors:      []int{400, 500},
		Middlewares: middlewares,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *BodyInput[T]) (*ItemOutput[T], error) {
		v := in.Body
		// Validate via configstore patch if provided.
		if k.Patch != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.Patch(v)); verr != nil {
				return nil, humaError(http.StatusBadRequest, verr.Error())
			}
		}
		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError, "begin tx: "+err.Error())
		}
		if err := k.Insert(ctx, tx, v); err != nil {
			_ = tx.Rollback(ctx)
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			return nil, humaError(http.StatusInternalServerError, "commit: "+err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after create; snapshot may be stale",
				"kind", k.Name, "name", k.ResourceID(v), "err", err)
			return nil, humaError(http.StatusInternalServerError,
				"mutation committed but reload failed: "+err.Error())
		}
		name := k.ResourceID(v)
		emitAuditCtx(deps.Logger, ctx, k.Name, name, "create", "")
		created, err := k.Get(ctx, name)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError,
				"created but could not read back: "+err.Error())
		}
		return &ItemOutput[T]{Body: created}, nil
	})

	// --- Update ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_update",
		Method:      http.MethodPut,
		Path:        nameParam,
		Summary:     "Update " + singular,
		Tags:        []string{"admin"},
		Errors:      []int{400, 404, 500},
		Middlewares: middlewares,
	}, func(ctx context.Context, in *NameBodyInput[T]) (*ItemOutput[T], error) {
		before, err := k.Get(ctx, in.Name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, humaError(http.StatusNotFound,
					fmt.Sprintf("%s %q not found", k.Name, in.Name))
			}
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		v := in.Body
		if k.Patch != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.Patch(v)); verr != nil {
				return nil, humaError(http.StatusBadRequest, verr.Error())
			}
		}
		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError, "begin tx: "+err.Error())
		}
		if err := k.Update(ctx, tx, in.Name, v); err != nil {
			_ = tx.Rollback(ctx)
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			return nil, humaError(http.StatusInternalServerError, "commit: "+err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after update; snapshot may be stale",
				"kind", k.Name, "name", in.Name, "err", err)
			return nil, humaError(http.StatusInternalServerError,
				"mutation committed but reload failed: "+err.Error())
		}
		diff := ""
		if k.Summarize != nil {
			diff = k.Summarize(before, v)
		}
		emitAuditCtx(deps.Logger, ctx, k.Name, in.Name, "update", diff)
		updated, err := k.Get(ctx, in.Name)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError,
				"updated but could not read back: "+err.Error())
		}
		return &ItemOutput[T]{Body: updated}, nil
	})

	// --- Delete ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_delete",
		Method:      http.MethodDelete,
		Path:        nameParam,
		Summary:     "Delete " + singular,
		Tags:        []string{"admin"},
		Errors:      []int{400, 404, 500},
		Middlewares: middlewares,
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *DeleteInput) (*struct{}, error) {
		if k.PatchDelete != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.PatchDelete(in.Name)); verr != nil {
				return nil, humaError(http.StatusBadRequest, verr.Error())
			}
		}
		tx, err := deps.Pool.Begin(ctx)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError, "begin tx: "+err.Error())
		}
		if err := k.Delete(ctx, tx, in.Name); err != nil {
			_ = tx.Rollback(ctx)
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if err := tx.Commit(ctx); err != nil {
			_ = tx.Rollback(ctx)
			return nil, humaError(http.StatusInternalServerError, "commit: "+err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.ErrorContext(ctx, "admin: reload failed after delete; snapshot may be stale",
				"kind", k.Name, "name", in.Name, "err", err)
			return nil, humaError(http.StatusInternalServerError,
				"mutation committed but reload failed: "+err.Error())
		}
		emitAuditCtx(deps.Logger, ctx, k.Name, in.Name, "delete", "")
		return nil, nil
	})
}

// emitAuditCtx is a context-only variant of emitAudit (no *http.Request available).
func emitAuditCtx(log interface {
	InfoContext(ctx context.Context, msg string, args ...any)
}, ctx context.Context, kind, name, action, diff string) {
	log.InfoContext(ctx, "admin: "+kind+" "+action,
		"kind", kind,
		"name", name,
		"action", action,
		"diff", diff,
	)
}
