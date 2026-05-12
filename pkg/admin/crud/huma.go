// Package crud — huma.go
//
// RegisterOps registers typed huma operations for a Kind[T]. The route surface
// per kind:
//
//	GET    {base}                 list
//	GET    {base}/{slugOrID}      read (slug or id; UUID form prefers id)
//	POST   {base}                 create (server stamps id+slug)
//	PUT    {base}/by-id/{id}      update (id-routed)
//	DELETE {base}/by-id/{id}      delete (id-routed)
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

// SlugOrIDInput is the path-param input for GET routes that accept either form.
type SlugOrIDInput struct {
	Ref string `path:"ref" doc:"Resource slug or id."`
}

// IDInput is the path-param input for id-routed PUT/DELETE.
type IDInput struct {
	ID string `path:"id" doc:"Resource id (UUIDv7)."`
}

// BodyInput is the request body input for create operations.
type BodyInput[T any] struct {
	Body T
}

// IDBodyInput combines id path param + request body for id-routed update.
type IDBodyInput[T any] struct {
	ID   string `path:"id" doc:"Resource id (UUIDv7)."`
	Body T
}

func humaError(status int, msg string) error {
	return huma.NewError(status, msg)
}

// RegisterOps registers the standard CRUD operations for Kind[T] on the huma API.
func RegisterOps[T any](
	api huma.API,
	base string, // e.g. "/control/providers"
	singular string, // e.g. "provider"
	plural string, // e.g. "providers"
	k *Kind[T],
	deps Deps,
	middlewares huma.Middlewares,
) {
	getPath := base + "/{ref}"
	idPath := base + "/by-id/{id}"

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

	// --- Get (slug or id) ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_get",
		Method:      http.MethodGet,
		Path:        getPath,
		Summary:     "Get " + singular + " by slug or id",
		Tags:        []string{"admin"},
		Errors:      []int{404, 500},
		Middlewares: middlewares,
	}, func(ctx context.Context, in *SlugOrIDInput) (*ItemOutput[T], error) {
		v, err := k.GetBySlugOrID(ctx, in.Ref)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, humaError(http.StatusNotFound,
					fmt.Sprintf("%s %q not found", k.Name, in.Ref))
			}
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		return &ItemOutput[T]{Body: v}, nil
	})

	// --- Create ---
	huma.Register(api, huma.Operation{
		OperationID:   "admin_" + singular + "_create",
		Method:        http.MethodPost,
		Path:          base,
		Summary:       "Create " + singular,
		Tags:          []string{"admin"},
		Errors:        []int{400, 500},
		Middlewares:   middlewares,
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *BodyInput[T]) (*ItemOutput[T], error) {
		v := in.Body
		if k.StampID != nil {
			if err := k.StampID(ctx, v); err != nil {
				return nil, humaError(http.StatusBadRequest, err.Error())
			}
		}
		if k.Patch != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.Patch(v)); verr != nil {
				return nil, humaError(http.StatusBadRequest, verr.Error())
			}
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return k.Insert(ctx, v)
		}); err != nil {
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.WarnContext(ctx, "admin: reload failed after create; snapshot may be stale",
				"kind", k.Name, "name", k.ResourceID(v), "err", err)
		}
		id := k.ResourceIDValue(v)
		emitAuditCtx(deps.Logger, ctx, k.Name, k.ResourceID(v), "create", "")
		created, err := k.GetByID(ctx, id)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError,
				"created but could not read back: "+err.Error())
		}
		return &ItemOutput[T]{Body: created}, nil
	})

	// --- Update (by id) ---
	huma.Register(api, huma.Operation{
		OperationID: "admin_" + singular + "_update",
		Method:      http.MethodPut,
		Path:        idPath,
		Summary:     "Update " + singular + " by id",
		Tags:        []string{"admin"},
		Errors:      []int{400, 404, 500},
		Middlewares: middlewares,
	}, func(ctx context.Context, in *IDBodyInput[T]) (*ItemOutput[T], error) {
		before, err := k.GetByID(ctx, in.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, humaError(http.StatusNotFound,
					fmt.Sprintf("%s with id %q not found", k.Name, in.ID))
			}
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if k.Guard != nil {
			if gerr := k.Guard(ctx, before, in.Body); gerr != nil {
				return nil, gerr
			}
		}
		v := in.Body
		if k.Patch != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.Patch(v)); verr != nil {
				return nil, humaError(http.StatusBadRequest, verr.Error())
			}
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return k.UpdateByID(ctx, in.ID, v)
		}); err != nil {
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.WarnContext(ctx, "admin: reload failed after update; snapshot may be stale",
				"kind", k.Name, "id", in.ID, "err", err)
		}
		diff := ""
		if k.Summarize != nil {
			diff = k.Summarize(before, v)
		}
		emitAuditCtx(deps.Logger, ctx, k.Name, k.ResourceID(v), "update", diff)
		updated, err := k.GetByID(ctx, in.ID)
		if err != nil {
			return nil, humaError(http.StatusInternalServerError,
				"updated but could not read back: "+err.Error())
		}
		return &ItemOutput[T]{Body: updated}, nil
	})

	// --- Delete (by id) ---
	huma.Register(api, huma.Operation{
		OperationID:   "admin_" + singular + "_delete",
		Method:        http.MethodDelete,
		Path:          idPath,
		Summary:       "Delete " + singular + " by id",
		Tags:          []string{"admin"},
		Errors:        []int{400, 404, 500},
		Middlewares:   middlewares,
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *IDInput) (*struct{}, error) {
		current, err := k.GetByID(ctx, in.ID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, humaError(http.StatusNotFound,
					fmt.Sprintf("%s with id %q not found", k.Name, in.ID))
			}
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if k.Guard != nil {
			var zero T
			if gerr := k.Guard(ctx, current, zero); gerr != nil {
				return nil, gerr
			}
		}
		slug := k.ResourceID(current)
		if k.PatchDelete != nil && deps.Patcher != nil {
			if verr := deps.Patcher.ValidateWithPatch(k.PatchDelete(slug)); verr != nil {
				return nil, humaError(http.StatusBadRequest, verr.Error())
			}
		}
		if err := deps.Tx.RunInTx(ctx, func(ctx context.Context) error {
			return k.DeleteByID(ctx, in.ID)
		}); err != nil {
			return nil, humaError(http.StatusInternalServerError, err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			deps.Logger.WarnContext(ctx, "admin: reload failed after delete; snapshot may be stale",
				"kind", k.Name, "id", in.ID, "err", err)
		}
		emitAuditCtx(deps.Logger, ctx, k.Name, slug, "delete", "")
		return nil, nil
	})
}

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
