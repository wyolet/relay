// Minting and revocation of inference tokens. Verification lives in the data
// plane, so nothing here is on the request path.

package control

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
)

// TokenDenylist writes the per-token revocation entries the data plane's
// Reserve script reads. Satisfied by kv.Store.
type TokenDenylist interface {
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
}

// tokenDeps is the slice of Deps the token handlers need, resolved once at
// mount so the handlers stay testable without a live store.
type tokenDeps struct {
	cfg      func() *settings.AuthTokens
	snap     func() *appcatalog.Snapshot
	signer   *TokenSigner
	denylist TokenDenylist
	users    tokenUserStore
	audit    audit.Reader
	authz    authz.Authorizer
	limiter  MintLimiter
}

// tokenUserStore is the narrow user-store surface minting and revocation
// need. *user.Store satisfies it; tests supply a fake.
type tokenUserStore interface {
	Get(ctx context.Context, id string) (*user.User, error)
	BumpTokenVersion(ctx context.Context, id string) error
}

func newTokenDeps(d Deps) tokenDeps {
	td := tokenDeps{
		cfg:      func() *settings.AuthTokens { return settings.AuthTokensFrom(d.Catalog) },
		snap:     d.Catalog.Current,
		signer:   d.TokenSigner,
		denylist: d.TokenDenylist,
		audit:    d.AuditReader,
		authz:    d.Authz,
		limiter:  d.MintLimiter,
	}
	if d.Users != nil {
		td.users = d.Users
	}
	return td
}

type mintTokenInput struct {
	Body struct {
		Project string `json:"project" minLength:"1" doc:"Project id or slug to mint the token for."`
		TTL     string `json:"ttl,omitempty" doc:"Requested lifetime as a Go duration (e.g. \"30m\"). Defaults to the auth:tokens defaultTTL; may not exceed maxTTL."`
	}
}

type mintTokenOutput struct {
	Body struct {
		Token     string    `json:"token"`
		JTI       string    `json:"jti"`
		ExpiresAt time.Time `json:"expiresAt"`
		Project   string    `json:"project"`
		// TeamID lets a caller revoke this jti even after the mint's audit
		// row is pruned or dropped — the denylist entry is keyed by team.
		TeamID string `json:"teamId"`
	}
}

type revokeTokenInput struct {
	Body struct {
		Token string `json:"token" minLength:"1" doc:"The token to revoke; only its claims are read."`
	}
}

func registerTokens(api huma.API, d Deps, protect huma.Middlewares) {
	td := newTokenDeps(d)

	huma.Register(api, huma.Operation{
		OperationID: "auth_token_mint",
		Method:      http.MethodPost,
		Path:        "/auth/token",
		Summary:     "Mint an inference token for a project",
		Description: "Session auth only: a token names the user it was minted for, " +
			"which an admin token cannot supply. The token is returned once and " +
			"never stored — revoke it by jti or bump the user's token version.",
		Tags:        []string{"auth"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 429, 500, 503},
	}, func(ctx context.Context, in *mintTokenInput) (*mintTokenOutput, error) {
		return mintToken(ctx, td, in)
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth_token_revoke_jti",
		Method:      http.MethodDelete,
		Path:        "/auth/token/{jti}",
		Summary:     "Revoke one inference token by its jti",
		Description: "The mint is looked up in the audit log, which is where the " +
			"token's project and expiry are recorded — no token registry exists.",
		Tags:        []string{"auth"},
		Middlewares: protect,
		Errors:      []int{401, 403, 404, 503},
	}, func(ctx context.Context, in *struct {
		JTI    string `path:"jti"`
		TeamID string `query:"team_id" doc:"Team the token was minted under, from the mint response. Used only when no mint is recorded; requires admin."`
		Exp    string `query:"exp"     doc:"Token expiry (RFC3339), from the mint response. Used only when no mint is recorded; requires admin."`
	}) (*emptyOutput, error) {
		return revokeTokenByJTI(ctx, td, in.JTI, in.TeamID, in.Exp)
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth_token_revoke",
		Method:      http.MethodPost,
		Path:        "/auth/token/revoke",
		Summary:     "Revoke the presented inference token",
		Description: "For a client holding the token but no session: the token " +
			"verifies itself, and its claims carry the project the denylist entry " +
			"is written under.",
		Tags:   []string{"auth"},
		Errors: []int{400, 401, 500, 503},
	}, func(ctx context.Context, in *revokeTokenInput) (*emptyOutput, error) {
		return revokeTokenByValue(ctx, td, in.Body.Token)
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth_token_revoke_all",
		Method:      http.MethodPost,
		Path:        "/auth/token/revoke-all",
		Summary:     "Invalidate every inference token the caller holds",
		Tags:        []string{"auth"},
		Middlewares: protect,
		Errors:      []int{401, 503},
	}, func(ctx context.Context, _ *struct{}) (*emptyOutput, error) {
		a := actor.From(ctx)
		if a == nil || a.UserID == "" {
			return nil, huma.Error401Unauthorized("a user session is required")
		}
		return bumpTokenVersion(ctx, td, a.UserID)
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth_token_key_rotate",
		Method:      http.MethodPost,
		Path:        "/auth/token/keys/rotate",
		Summary:     "Rotate the inference-token signing key",
		Description: "Generates a new signing key and records it in the auth:tokens " +
			"section. The outgoing key stays on the verifier, so tokens already " +
			"minted keep working until they expire.",
		Tags:        []string{"auth"},
		Middlewares: protect,
		Errors:      []int{401, 403, 500, 503},
	}, func(ctx context.Context, _ *struct{}) (*emptyOutput, error) {
		return rotateTokenKey(ctx, d)
	})

	huma.Register(api, huma.Operation{
		OperationID: "user_revoke_tokens",
		Method:      http.MethodPost,
		Path:        "/users/by-id/{id}/revoke-tokens",
		Summary:     "Invalidate every inference token a user holds",
		Tags:        []string{"users"},
		Middlewares: protect,
		Errors:      []int{401, 403, 404, 503},
	}, func(ctx context.Context, in *struct {
		ID string `path:"id"`
	}) (*emptyOutput, error) {
		if err := td.authz.Authorize(ctx, "users.update", authz.Resource{Kind: "user", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		return bumpTokenVersion(ctx, td, in.ID)
	})
}
