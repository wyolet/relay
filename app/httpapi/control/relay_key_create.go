// Custom POST /relay-keys: generates the bearer plaintext server-side
// via relaykey.Generate, persists only the hash + prefix, and returns
// the plaintext exactly once on the create response. The generic CRUD
// POST in registerKind is skipped for this kind (skipCreate=true) so
// callers can't sneak a precomputed keyHash through.
package control

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/slug"
)

type createRelayKeyInput struct {
	Body struct {
		Metadata struct {
			Name        string `json:"name,omitempty"`
			DisplayName string `json:"displayName,omitempty"`
		} `json:"metadata"`
		Spec struct {
			PolicyID           string `json:"policyId,omitempty"`
			Enabled            *bool  `json:"enabled,omitempty"`
			PassthroughAllowed bool   `json:"passthroughAllowed,omitempty"`
		} `json:"spec"`
	} `json:"body"`
}

type createRelayKeyResponse struct {
	Body struct {
		// Plaintext is the bearer token the caller must use as the
		// inbound API key. Returned exactly once on create — never
		// retrievable again. Persist this on the client side
		// immediately.
		Plaintext string             `json:"plaintext"`
		RelayKey  *relaykey.RelayKey `json:"relayKey"`
	}
}

func registerRelayKeyCreate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "create_relay_key",
		Method:      http.MethodPost,
		Path:        "/relay-keys",
		Summary:     "Create a relay-key (server generates plaintext)",
		Description: "Generates a fresh bearer token server-side via crypto/rand, " +
			"persists only sha256(plaintext) + a short display prefix, and returns " +
			"the plaintext once in the response. The caller MUST save the plaintext " +
			"on receipt — it is not retrievable later.",
		Tags:          []string{"relay-keys"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 500},
	}, func(ctx context.Context, in *createRelayKeyInput) (*createRelayKeyResponse, error) {
		if err := d.Authz.Authorize(ctx, "relay-keys.create", authz.Resource{Kind: "relay-key"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		gen, err := relaykey.Generate()
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		k := &relaykey.RelayKey{}
		k.Meta.ID = ids.New()
		k.Meta.DisplayName = in.Body.Metadata.DisplayName
		k.Meta.Name = in.Body.Metadata.Name
		if k.Meta.Name == "" {
			base := slug.From(k.Meta.DisplayName)
			if base == "" {
				base = "relay-key"
			}
			k.Meta.Name = slug.Unique(base, slugTakenFn(d.Stores.RelayKey, func(rk *relaykey.RelayKey) *meta.Metadata { return &rk.Meta }))
		}
		k.Meta.Owner.Kind = meta.OwnerUser

		k.Spec.PolicyID = in.Body.Spec.PolicyID
		k.Spec.Enabled = in.Body.Spec.Enabled
		k.Spec.PassthroughAllowed = in.Body.Spec.PassthroughAllowed
		k.Spec.KeyHash = gen.KeyHash
		k.Spec.Prefix = gen.Prefix

		if err := k.Validate(); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := d.Stores.RelayKey.Upsert(ctx, k); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		created, err := d.Stores.RelayKey.Get(ctx, k.Meta.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("created but could not read back: " + err.Error())
		}

		out := &createRelayKeyResponse{}
		out.Body.Plaintext = gen.Plaintext
		out.Body.RelayKey = created
		return out, nil
	})
}
