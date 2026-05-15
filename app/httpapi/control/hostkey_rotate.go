package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/hostkey"
)

// POST /host-keys/by-id/{id}/rotate — replace the stored-mode credential
// without touching any other metadata. PUT cannot rotate (guardHostKey
// rejects value on update) so the rotate-vs-edit distinction is explicit
// and a stray value field never causes an accidental rotation.
//
// Body: {"value": "<new-cleartext>"}. Env-mode keys can't rotate this way —
// flip the env var instead — and the handler returns 400.
type rotateHostKeyInput struct {
	ID   string `path:"id" doc:"HostKey id (UUIDv7)."`
	Body struct {
		Value string `json:"value" doc:"New cleartext credential."`
	}
}

func registerHostKeyRotate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "rotate_host_key",
		Method:      http.MethodPost,
		Path:        "/host-keys/by-id/{id}/rotate",
		Summary:     "Rotate a stored-mode HostKey credential",
		Tags:        []string{"host-keys"},
		Middlewares: protect,
		Errors:      []int{400, 401, 404, 500},
	}, func(ctx context.Context, in *rotateHostKeyInput) (*itemResponse[hostkey.HostKey], error) {
		if err := d.Authz.Authorize(ctx, "host-keys.update", authz.Resource{Kind: "host-key", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if in.Body.Value == "" {
			return nil, huma.Error400BadRequest("value is required")
		}
		existing, err := d.Stores.HostKey.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("host-key %q not found", in.ID))
		}
		if existing.Spec.ValueFrom.Kind != hostkey.ValueKindStored {
			return nil, huma.Error400BadRequest("rotate is supported only for stored-mode host-keys")
		}
		existing.Spec.Value = in.Body.Value
		if err := d.Stores.HostKey.Upsert(ctx, existing); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		rotated, err := d.Stores.HostKey.Get(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("rotated but could not read back: " + err.Error())
		}
		return &itemResponse[hostkey.HostKey]{Body: rotated}, nil
	})
}
