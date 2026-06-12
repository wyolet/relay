// POST /relay-keys/by-id/{id}/rotate — mint a fresh bearer plaintext for
// an existing relay-key, replacing KeyHash + Prefix in place. All other
// fields (policy binding, flags, slug) survive, so customers swap the
// secret without re-wiring anything. The old plaintext stops
// authenticating as soon as the snapshot picks up the NOTIFY (~1s
// fleet-wide). Like create, the new plaintext is returned exactly once.
package control

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/relaykey"
)

type rotateRelayKeyInput struct {
	ID string `path:"id" doc:"RelayKey id (UUIDv7)."`
}

type rotateRelayKeyResponse struct {
	Body struct {
		// Plaintext is the new bearer token. Returned exactly once —
		// never retrievable again. The previous token is invalid.
		Plaintext string             `json:"plaintext"`
		RelayKey  *relaykey.RelayKey `json:"relayKey"`
	}
}

func registerRelayKeyRotate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "rotate_relay_key",
		Method:      http.MethodPost,
		Path:        "/relay-keys/by-id/{id}/rotate",
		Summary:     "Rotate a relay-key (server generates a new plaintext)",
		Description: "Generates a fresh bearer token server-side, replaces the stored " +
			"hash + display prefix, and returns the new plaintext once. The old token " +
			"stops authenticating within ~1s fleet-wide. Revoked keys cannot be " +
			"rotated — create a new key instead.",
		Tags:        []string{"relay-keys"},
		Middlewares: protect,
		Errors:      []int{400, 401, 404, 500},
	}, func(ctx context.Context, in *rotateRelayKeyInput) (*rotateRelayKeyResponse, error) {
		if err := d.Authz.Authorize(ctx, "relay-keys.update", authz.Resource{Kind: "relay-key", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		existing, err := d.Stores.RelayKey.Get(ctx, in.ID)
		if err != nil || existing == nil {
			return nil, huma.Error404NotFound(fmt.Sprintf("relay-key %q not found", in.ID))
		}
		// Rotating a revoked key would hand out a token that still can't
		// authenticate (RevokedAt survives rotation) — reject instead of
		// minting a dead credential.
		if existing.Spec.RevokedAt != nil {
			return nil, huma.Error400BadRequest("relay-key is revoked; create a new key instead of rotating")
		}
		gen, err := relaykey.Generate()
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		existing.Spec.KeyHash = gen.KeyHash
		existing.Spec.Prefix = gen.Prefix
		if err := d.Stores.RelayKey.Upsert(ctx, existing); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		rotated, err := d.Stores.RelayKey.Get(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("rotated but could not read back: " + err.Error())
		}
		out := &rotateRelayKeyResponse{}
		out.Body.Plaintext = gen.Plaintext
		out.Body.RelayKey = rotated
		return out, nil
	})
}
