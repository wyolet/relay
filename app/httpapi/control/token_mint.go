package control

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/ids"
)

func mintToken(ctx context.Context, d tokenDeps, in *mintTokenInput) (*mintTokenOutput, error) {
	cfg := d.cfg()
	if d.signer == nil || !cfg.Enabled || d.signer.PublicKey() == nil {
		return nil, huma.Error503ServiceUnavailable("tokens_disabled: no token signing key is configured")
	}
	a := actor.From(ctx)
	if a == nil || a.UserID == "" {
		// An admin token authenticates a machine, not a user, and a token
		// must name the user it acts as.
		return nil, huma.Error400BadRequest("minting a token requires a user session")
	}

	snap := d.snap()
	proj, ok := snap.Project(in.Body.Project)
	if !ok {
		proj, ok = snap.ProjectByName(in.Body.Project)
	}
	if !ok {
		return nil, huma.Error404NotFound("project not found")
	}
	owner := meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}
	if err := d.authz.Authorize(ctx, "tokens.mint", authz.Resource{Kind: "token", Owner: &owner}); err != nil {
		if errors.Is(err, authz.ErrUnauthenticated) {
			return nil, mapAuthzErr(err)
		}
		// Same answer as an unknown project: a 403 here would confirm which
		// project slugs exist to anyone with a session.
		return nil, huma.Error404NotFound("project not found")
	}
	if err := reserveMint(ctx, d, a.UserID); err != nil {
		return nil, err
	}

	ttl := cfg.DefaultTTL
	if cfg.MaxTTL > 0 && ttl > cfg.MaxTTL {
		// A default above the maximum is a misconfiguration, not a licence to
		// exceed it.
		ttl = cfg.MaxTTL
	}
	if in.Body.TTL != "" {
		parsed, err := time.ParseDuration(in.Body.TTL)
		if err != nil || parsed <= 0 {
			return nil, huma.Error400BadRequest("ttl must be a positive Go duration")
		}
		if parsed > cfg.MaxTTL {
			return nil, huma.Error400BadRequest("ttl exceeds the configured maximum of " + cfg.MaxTTL.String())
		}
		ttl = parsed
	}

	version := 0
	if d.users != nil {
		u, err := d.users.Get(ctx, a.UserID)
		if err != nil {
			return nil, huma.Error500InternalServerError("user lookup failed: " + err.Error())
		}
		if u == nil {
			return nil, huma.Error404NotFound("user not found")
		}
		if u.Disabled {
			return nil, huma.Error403Forbidden("account is disabled")
		}
		version = u.TokenVersion
	}

	now := time.Now()
	exp := now.Add(ttl)
	claims := crypto.TokenClaims{
		Iss: crypto.TokenIssuer,
		Sub: "user:" + a.UserID,
		Prj: proj.Meta.ID,
		Grp: mintGroups(snap, a),
		Ver: version,
		Jti: ids.New(),
		Iat: now.Unix(),
		Exp: exp.Unix(),
	}
	token, err := d.signer.sign(claims)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("tokens_disabled: " + err.Error())
	}

	// The expiry rides the audit row's resource name: revoke-by-jti needs it
	// to bound the denylist entry, and there is no token registry to read.
	audit.Record(ctx, "tokens.mint", audit.Resource{
		Kind:  "token",
		ID:    claims.Jti,
		Name:  exp.UTC().Format(time.RFC3339),
		Owner: &owner,
		Scope: []string{"project:" + proj.Meta.ID, "team:" + proj.Spec.TeamID},
	}, audit.StatusAllowed)

	out := &mintTokenOutput{}
	out.Body.Token = token
	out.Body.JTI = claims.Jti
	out.Body.ExpiresAt = exp.UTC()
	out.Body.Project = proj.Meta.Name
	out.Body.TeamID = proj.Spec.TeamID
	return out, nil
}

// mintGroups is the group set the token carries: what the IdP asserted at
// login plus the local groups holding this user.
func mintGroups(snap *appcatalog.Snapshot, a *actor.Actor) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a.IdPGroups))
	for _, g := range append(append([]string(nil), a.IdPGroups...), snap.GroupsForUser(a.UserID)...) {
		if _, dup := seen[g]; dup || g == "" {
			continue
		}
		seen[g] = struct{}{}
		out = append(out, g)
	}
	return out
}
