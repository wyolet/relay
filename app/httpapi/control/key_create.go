// Custom POST /keys: generates the bearer plaintext server-side
// via key.Generate, persists only the hash + prefix, and returns
// the plaintext exactly once on the create response. The generic CRUD
// POST in registerKind is skipped for this kind (skipCreate=true) so
// callers can't sneak a precomputed keyHash through.
package control

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/slug"
)

type createKeyInput struct {
	Body struct {
		Metadata struct {
			Name        string `json:"name,omitempty"`
			DisplayName string `json:"displayName,omitempty"`
		} `json:"metadata"`
		Spec struct {
			Principal             key.Principal `json:"principal"`
			PolicyID              string        `json:"policyId,omitempty"`
			ExpiresAt             *time.Time    `json:"expiresAt,omitempty"`
			Enabled               *bool         `json:"enabled,omitempty"`
			PassthroughAllowed    bool          `json:"passthroughAllowed,omitempty"`
			PayloadLoggingEnabled bool          `json:"payloadLoggingEnabled,omitempty"`
		} `json:"spec"`
	} `json:"body"`
}

type createKeyResponse struct {
	Body struct {
		// Plaintext is the bearer token the caller must use as the
		// inbound API key. Returned exactly once on create — never
		// retrievable again. Persist this on the client side
		// immediately.
		Plaintext string   `json:"plaintext"`
		Key       *key.Key `json:"key"`
	}
}

func registerKeyCreate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "create_relay_key",
		Method:      http.MethodPost,
		Path:        "/keys",
		Summary:     "Create a key (server generates plaintext)",
		Description: "Generates a fresh bearer token server-side via crypto/rand, " +
			"persists only sha256(plaintext) + a short display prefix, and returns " +
			"the plaintext once in the response. The caller MUST save the plaintext " +
			"on receipt — it is not retrievable later.",
		Tags:          []string{"keys"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *createKeyInput) (*createKeyResponse, error) {
		gen, err := key.Generate()
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}

		k := &key.Key{}
		k.Meta.ID = ids.New()
		k.Meta.DisplayName = in.Body.Metadata.DisplayName
		k.Meta.Name = in.Body.Metadata.Name
		if k.Meta.Name == "" {
			base := slug.From(k.Meta.DisplayName)
			if base == "" {
				base = "key"
			}
			k.Meta.Name = slug.Unique(base, slugTakenFn(d.Stores.Key, func(rk *key.Key) *meta.Metadata { return &rk.Meta }))
		}
		k.Spec.Principal = in.Body.Spec.Principal
		if err := refs(d).KeyOwner(ctx, k); err != nil {
			return nil, refErr(err)
		}
		// Authorize AFTER owner stamping, same as the generic create path.
		if err := d.Authz.Authorize(ctx, "keys.create", authz.Resource{Kind: "key", Owner: &k.Meta.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if err := checkPolicyRefVisible(ctx, d, in.Body.Spec.PolicyID, k.Meta.Owner); err != nil {
			return nil, err
		}

		k.Spec.PolicyID = in.Body.Spec.PolicyID
		k.Spec.ExpiresAt = in.Body.Spec.ExpiresAt
		k.Spec.Enabled = in.Body.Spec.Enabled
		k.Spec.PassthroughAllowed = in.Body.Spec.PassthroughAllowed
		k.Spec.PayloadLoggingEnabled = in.Body.Spec.PayloadLoggingEnabled
		k.Spec.KeyHash = gen.KeyHash
		k.Spec.Prefix = gen.Prefix

		if err := k.Validate(); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := d.Stores.Key.Upsert(ctx, k); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		created, err := d.Stores.Key.Get(ctx, k.Meta.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("created but could not read back: " + err.Error())
		}

		out := &createKeyResponse{}
		out.Body.Plaintext = gen.Plaintext
		out.Body.Key = created
		return out, nil
	})
}
