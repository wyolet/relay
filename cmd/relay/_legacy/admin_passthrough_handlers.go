package main

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/admin/crud"
)

// registerPassthroughOps mounts the singleton GET/PUT pair for the
// Passthrough resource. There's no POST/DELETE — GET always returns a value
// (DefaultPassthrough() when no row exists), and PUT is full-replace.
func registerPassthroughOps(
	api huma.API,
	store *catalog.PGStore,
	deps *crud.Deps,
	adminAuth huma.Middlewares,
) {
	type out struct {
		Body catalog.Passthrough
	}
	type putInput struct {
		Body catalog.Passthrough
	}

	huma.Register(api, huma.Operation{
		OperationID: "admin_passthrough_get",
		Method:      http.MethodGet,
		Path:        "/control/passthrough",
		Summary:     "Get the Passthrough singleton config",
		Description: "Returns the current Passthrough config, or the safe-by-default config when none has been written.",
		Tags:        []string{"control"},
		Middlewares: adminAuth,
	}, func(_ context.Context, _ *struct{}) (*out, error) {
		o := &out{Body: *store.Passthrough()}
		return o, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin_passthrough_put",
		Method:      http.MethodPut,
		Path:        "/control/passthrough",
		Summary:     "Replace the Passthrough singleton config",
		Description: "Full-replace semantics. Body is the complete spec; partial updates are not supported.",
		Tags:        []string{"control"},
		Errors:      []int{400, 401, 500},
		Middlewares: adminAuth,
	}, func(ctx context.Context, in *putInput) (*out, error) {
		v := in.Body
		// Stamp the singleton invariants — clients don't need to set them.
		v.APIVersion = catalog.APIVersion
		v.Kind = catalog.KindPassthrough
		if v.Metadata.Name == "" {
			v.Metadata.Name = catalog.PassthroughSingletonName
		}
		if err := deps.Patcher.ValidateWithPatch(catalog.Patch{SetPassthrough: &v}); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := store.SetPassthrough(ctx, v); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &out{Body: *store.Passthrough()}, nil
	})
}
