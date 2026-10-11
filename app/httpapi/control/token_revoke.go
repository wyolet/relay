package control

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/pkg/crypto"
)

// revokeTokenByJTI denies one token. The audit row of its mint is the lookup:
// it carries the project and the expiry, and there is no token registry.
// teamHint/expHint are the same two facts as returned by mint; they stand in
// when the audit row is gone (dropped under load, or pruned). Because nothing
// then proves who minted the token, the hint path is admin-only.
func revokeTokenByJTI(ctx context.Context, d tokenDeps, jti, teamHint, expHint string) (*emptyOutput, error) {
	var ev audit.Event
	if d.audit != nil {
		// ResourceKinds narrows to the indexed prefix; the resource-id
		// predicate on its own is a sequential scan of the log.
		events, err := d.audit.Events(ctx, audit.Query{
			Actions:       []string{"tokens.mint"},
			ResourceKinds: []string{"token"},
			ResourceID:    jti,
			Limit:         1,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError("audit lookup failed: " + err.Error())
		}
		if len(events) > 0 {
			ev = events[0]
		}
	}
	if ev.Resource.Owner == nil {
		if teamHint == "" || expHint == "" {
			return nil, huma.Error404NotFound("no mint recorded for this token")
		}
		if !authz.IsAdmin(ctx) {
			return nil, huma.Error403Forbidden("revoking without a mint record requires an admin caller")
		}
		exp, err := time.Parse(time.RFC3339, expHint)
		if err != nil {
			return nil, huma.Error400BadRequest("exp must be an RFC3339 timestamp")
		}
		return denyToken(ctx, d, teamHint, jti, exp)
	}
	if err := d.authz.Authorize(ctx, "tokens.revoke",
		authz.Resource{Kind: "token", ID: jti, Owner: ev.Resource.Owner}); err != nil {
		// The minting user may always revoke their own token, whatever the
		// bindings say.
		if a := actor.From(ctx); a == nil || a.UserID == "" || a.UserID != ev.Actor.ID {
			return nil, mapAuthzErr(err)
		}
	}
	proj, ok := d.snap().Project(ev.Resource.Owner.ID)
	if !ok {
		return nil, huma.Error404NotFound("the token's project is no longer available")
	}
	exp, err := time.Parse(time.RFC3339, ev.Resource.Name)
	if err != nil {
		return nil, huma.Error500InternalServerError("the mint record carries no usable expiry")
	}
	return denyToken(ctx, d, proj.Spec.TeamID, jti, exp)
}

func revokeTokenByValue(ctx context.Context, d tokenDeps, raw string) (*emptyOutput, error) {
	if d.signer == nil {
		return nil, huma.Error503ServiceUnavailable("tokens_disabled: no token signing key is configured")
	}
	keys := d.signer.verificationKeys()
	if len(keys) == 0 {
		return nil, huma.Error503ServiceUnavailable("tokens_disabled: no token signing key is configured")
	}
	// A token minted before a rotation must still be revocable, so the
	// retired key is tried too.
	var claims crypto.TokenClaims
	var err error
	for _, pub := range keys {
		if claims, err = authtoken.Parse[crypto.TokenClaims](pub, raw); err == nil {
			break
		}
	}
	if err != nil {
		return nil, huma.Error401Unauthorized("invalid token")
	}
	proj, ok := d.snap().Project(claims.Prj)
	if !ok {
		return nil, huma.Error401Unauthorized("the token's project is no longer available")
	}
	return denyToken(ctx, d, proj.Spec.TeamID, claims.Jti, time.Unix(claims.Exp, 0))
}

// denyToken writes the denylist entry the inbound Reserve script checks. Its
// TTL is the token's remaining life: past that the claims expire anyway.
func denyToken(ctx context.Context, d tokenDeps, teamID, jti string, exp time.Time) (*emptyOutput, error) {
	remaining := time.Until(exp)
	if remaining <= 0 {
		// Already expired — nothing to deny, and a zero TTL would be a
		// permanent key.
		audit.Record(ctx, "tokens.revoke", audit.Resource{Kind: "token", ID: jti}, audit.StatusAllowed)
		return &emptyOutput{}, nil
	}
	if d.denylist == nil {
		return nil, huma.Error503ServiceUnavailable("no kv store is configured for token revocation")
	}
	if err := d.denylist.Set(ctx, policy.RevokedKey(teamID, jti), []byte("1"), remaining); err != nil {
		return nil, huma.Error500InternalServerError("revocation write failed: " + err.Error())
	}
	audit.Record(ctx, "tokens.revoke", audit.Resource{
		Kind:  "token",
		ID:    jti,
		Owner: &meta.Owner{Kind: meta.OwnerTeam, ID: teamID},
	}, audit.StatusAllowed)
	return &emptyOutput{}, nil
}

func bumpTokenVersion(ctx context.Context, d tokenDeps, userID string) (*emptyOutput, error) {
	if d.users == nil {
		return nil, huma.Error503ServiceUnavailable("no user store is configured")
	}
	u, err := d.users.Get(ctx, userID)
	if err != nil {
		return nil, huma.Error500InternalServerError("user lookup failed: " + err.Error())
	}
	if u == nil {
		return nil, huma.Error404NotFound("user not found")
	}
	if err := d.users.BumpTokenVersion(ctx, userID); err != nil {
		return nil, huma.Error500InternalServerError("token version bump failed: " + err.Error())
	}
	audit.Record(ctx, "tokens.revoke-all", audit.Resource{Kind: "user", ID: userID, Name: u.Username}, audit.StatusAllowed)
	return &emptyOutput{}, nil
}
