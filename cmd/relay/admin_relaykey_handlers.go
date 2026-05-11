package main

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/pkg/admin/crud"
)

// registerRelayKeyRevokeRestoreOps mounts the two convenience endpoints that
// flip Spec.RevokedAt on a RelayKey. Both reuse the standard upsert path so
// validation and snapshot reload happen the same way as a regular CRUD update.
func registerRelayKeyRevokeRestoreOps(
	api huma.API,
	store *catalog.PGStore,
	deps *crud.Deps,
	adminAuth huma.Middlewares,
) {
	type pathInput struct {
		Name string `path:"name" doc:"Relay key name."`
	}
	type out struct {
		Body struct {
			Name      string     `json:"name"`
			RevokedAt *time.Time `json:"revokedAt,omitempty"`
		}
	}

	flip := func(ctx context.Context, name string, setRevoked bool) (*out, error) {
		existing, ok := store.RelayKeyByName(name)
		if !ok {
			return nil, huma.Error404NotFound("relay key not found")
		}
		updated := *existing
		spec := existing.Spec
		if setRevoked {
			now := time.Now().UTC()
			spec.RevokedAt = &now
		} else {
			spec.RevokedAt = nil
		}
		updated.Spec = spec
		if err := deps.Patcher.ValidateWithPatch(catalog.Patch{UpsertRelayKey: &updated}); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := store.UpsertRelayKey(ctx, updated); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if err := deps.Reloader.Reload(ctx); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		o := &out{}
		o.Body.Name = updated.Metadata.Name
		o.Body.RevokedAt = updated.Spec.RevokedAt
		return o, nil
	}

	huma.Register(api, huma.Operation{
		OperationID: "admin_key_revoke",
		Method:      http.MethodPost,
		Path:        "/control/keys/{name}/revoke",
		Summary:     "Revoke a relay key",
		Tags:        []string{"control"},
		Middlewares: adminAuth,
	}, func(ctx context.Context, in *pathInput) (*out, error) {
		return flip(ctx, in.Name, true)
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin_key_restore",
		Method:      http.MethodPost,
		Path:        "/control/keys/{name}/restore",
		Summary:     "Restore (unrevoke) a relay key",
		Tags:        []string{"control"},
		Middlewares: adminAuth,
	}, func(ctx context.Context, in *pathInput) (*out, error) {
		return flip(ctx, in.Name, false)
	})
}
