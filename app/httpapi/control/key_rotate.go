// POST /keys/by-id/{id}/rotate — mint a fresh bearer plaintext for an
// existing key, replacing KeyHash + Prefix in place. All other fields
// (principal, policy binding, flags, slug) survive, so customers swap the
// secret without re-wiring anything. With graceSeconds > 0 the previous
// plaintext keeps authenticating until the window closes; with 0 it stops
// as soon as the snapshot picks up the NOTIFY (~1s fleet-wide). Like
// create, the new plaintext is returned exactly once.
package control

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/settings"
)

type rotateKeyBody struct {
	// GraceSeconds keeps the previous plaintext valid for this long.
	// 0 (the default) cuts over immediately.
	GraceSeconds int `json:"graceSeconds,omitempty"`
}

type rotateKeyInput struct {
	ID string `path:"id" doc:"Key id (UUIDv7)."`
	// Pointer so an omitted body still means an immediate cut-over.
	Body *rotateKeyBody `json:"body,omitempty" required:"false"`
}

type rotateKeyResponse struct {
	Body struct {
		// Plaintext is the new bearer token. Returned exactly once —
		// never retrievable again.
		Plaintext string   `json:"plaintext"`
		Key       *key.Key `json:"key"`
	}
}

func registerKeyRotate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "rotate_relay_key",
		Method:      http.MethodPost,
		Path:        "/keys/by-id/{id}/rotate",
		Summary:     "Rotate a key (server generates a new plaintext)",
		Description: "Generates a fresh bearer token server-side, replaces the stored " +
			"hash + display prefix, and returns the new plaintext once. The old token " +
			"stops authenticating after graceSeconds (0 = immediately, within ~1s " +
			"fleet-wide). Revoked keys cannot be rotated — create a new key instead.",
		Tags:        []string{"keys"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *rotateKeyInput) (*rotateKeyResponse, error) {
		existing, err := d.Stores.Key.Get(ctx, in.ID)
		if err != nil || existing == nil || !visibleTo(ctx, d.Authz, "key", existing.Meta.ID, existing.Meta.Owner) {
			return nil, huma.Error404NotFound(fmt.Sprintf("key %q not found", in.ID))
		}
		if err := d.Authz.Authorize(ctx, "keys.update", authz.Resource{Kind: "key", ID: in.ID, Owner: &existing.Meta.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		// Rotating a revoked key would hand out a token that still can't
		// authenticate (RevokedAt survives rotation) — reject instead of
		// minting a dead credential.
		if existing.Spec.RevokedAt != nil {
			return nil, huma.Error400BadRequest("key is revoked; create a new key instead of rotating")
		}
		grace := 0
		if in.Body != nil {
			grace = in.Body.GraceSeconds
		}
		if grace < 0 {
			return nil, huma.Error400BadRequest("graceSeconds must not be negative")
		}
		if max := maxRotateGrace(d); grace > max {
			return nil, huma.Error400BadRequest(fmt.Sprintf("graceSeconds %d exceeds the configured maximum of %d", grace, max))
		}
		gen, err := key.Generate()
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		audit.Changed(ctx, []string{"spec.keyHash", "spec.prefix", "spec.previousKeyHash", "spec.graceUntil"})
		if grace > 0 {
			until := time.Now().Add(time.Duration(grace) * time.Second)
			existing.Spec.PreviousKeyHash = existing.Spec.KeyHash
			existing.Spec.GraceUntil = &until
		} else {
			existing.Spec.PreviousKeyHash = ""
			existing.Spec.GraceUntil = nil
		}
		existing.Spec.KeyHash = gen.KeyHash
		existing.Spec.Prefix = gen.Prefix
		if err := d.Stores.Key.Upsert(ctx, existing); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		rotated, err := d.Stores.Key.Get(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("rotated but could not read back: " + err.Error())
		}
		out := &rotateKeyResponse{}
		out.Body.Plaintext = gen.Plaintext
		out.Body.Key = rotated
		return out, nil
	})
}

func maxRotateGrace(d Deps) int {
	if d.Catalog == nil {
		return settings.DefaultKeyRotateMaxGrace
	}
	v, ok := d.Catalog.Setting(settings.SectionInference)
	if !ok {
		return settings.DefaultKeyRotateMaxGrace
	}
	inf, ok := v.(*settings.Inference)
	if !ok {
		return settings.DefaultKeyRotateMaxGrace
	}
	return inf.MaxRotateGrace()
}
