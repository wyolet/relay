// Keys skip the generic CRUD create (skipCreate=true) so a caller cannot
// supply a precomputed keyHash: the plaintext is generated here, only its
// hash is stored, and it is returned exactly once.

package control

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/slug"
)

type createKeyMetadata struct {
	Name        string `json:"name,omitempty"        doc:"Slug for the key; derived from displayName when omitted."`
	DisplayName string `json:"displayName,omitempty" doc:"Free-text name shown in the UI."`
}

type createKeySpec struct {
	Principal             key.Principal `json:"principal,omitempty"             doc:"The service account or user the key acts as. Its owner becomes the key's owner. Omitted: the signed-in user; the admin token must name one."`
	AccountID             string        `json:"accountId,omitempty"             format:"uuid" doc:"Id of the account the key's requests run in by default: the service account's account, the user's personal account, a member account a role binding grants the user, or a provider account the user owns personally. Any other answers 403 account_not_held. Omitted: the service account's account, or the user's personal account. While the user's personal accounts are turned off, omitting it answers 400 lane_required, and naming the personal account or one of the user's provider accounts 403 personal_account_off."`
	ExpiresAt             *time.Time    `json:"expiresAt,omitempty"             doc:"When the key stops authenticating. Omitted never expires."`
	Enabled               *bool         `json:"enabled,omitempty"               doc:"False creates the key disabled. Omitted means true."`
	PassthroughAllowed    bool          `json:"passthroughAllowed,omitempty"    doc:"Let the key send its own upstream credential in proxy mode."`
	PayloadLoggingEnabled bool          `json:"payloadLoggingEnabled,omitempty" doc:"Capture request and response bodies of this key's requests. An account on the request's ladder that sets payloadLogging decides instead."`
}

type createKeyInput struct {
	Body struct {
		Metadata createKeyMetadata `json:"metadata" doc:"Name of the new key."`
		Spec     createKeySpec     `json:"spec"     doc:"The new key's principal and flags."`
	} `json:"body"`
}

type createKeyResponse struct {
	Body struct {
		// Plaintext is the bearer token the caller must use as the
		// inbound API key. Returned exactly once on create — never
		// retrievable again. Persist this on the client side
		// immediately.
		Plaintext string   `json:"plaintext" doc:"The bearer token to send as Authorization: Bearer. Returned only here; store it now."`
		Key       *key.Key `json:"key"       doc:"The stored key."`
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
		gen, err := apikey.Generate(key.TokenPrefix)
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
		k.Spec.Principal = key.Principal{Kind: in.Body.Spec.Principal.Kind, ID: in.Body.Spec.Principal.ID}
		if k.Spec.Principal == (key.Principal{}) {
			a := actor.From(ctx)
			if a == nil || a.AdminToken || a.UserID == "" {
				return nil, huma.Error400BadRequest("spec.principal is required here: the admin token is no user, so name the service account or user the key acts as")
			}
			k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: a.UserID}
		}
		if err := refs(d).KeyOwner(ctx, k); err != nil {
			return nil, refErr(err)
		}
		// Authorize AFTER owner stamping, same as the generic create path.
		if err := d.Authz.Authorize(ctx, "keys.create", authz.Resource{Kind: "key", Owner: &k.Meta.Owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		k.Spec.AccountID = in.Body.Spec.AccountID
		if err := refs(d).KeyAccount(ctx, k); err != nil {
			return nil, refErr(err)
		}
		k.Spec.ExpiresAt = in.Body.Spec.ExpiresAt
		k.Spec.Enabled = in.Body.Spec.Enabled
		k.Spec.PassthroughAllowed = in.Body.Spec.PassthroughAllowed
		k.Spec.PayloadLoggingEnabled = in.Body.Spec.PayloadLoggingEnabled
		k.Spec.KeyHash = gen.Hash
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
		// The plaintext is often spent at once; answer when this pod authenticates it.
		if d.Plans != nil {
			awaitServed(ctx, "key", k.Meta.ID, func(ctx context.Context) bool { return d.Plans.AwaitKey(ctx, k.Spec.KeyHash) })
		}

		out := &createKeyResponse{}
		out.Body.Plaintext = gen.Plaintext
		enrichKey(d)(ctx, created)
		out.Body.Key = created
		return out, nil
	})
}
