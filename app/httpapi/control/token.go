// token.go is the control plane's half of inference tokens: mint one for a
// project the caller can see, and revoke — one token by jti or by the token
// itself, or every token a user holds. Verification lives in the data plane;
// nothing here is on the request path.
package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/ids"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// TokenSigner holds the Ed25519 signing key. The composition root swaps the
// key in when the auth:tokens section changes; an empty signer refuses to
// mint, which is the posture of a deployment with no master key.
type TokenSigner struct {
	key atomic.Pointer[signingKey]
	// prev is the verification half of the key a rotation retired: minting
	// never uses it, but a token presented for revocation may carry it.
	prev atomic.Pointer[ed25519.PublicKey]
}

// signingKey pairs the private half with the `kid` the tokens it signs
// carry, so the two can never be read out of step.
type signingKey struct {
	priv ed25519.PrivateKey
	kid  string
}

// SetSeed installs the signing key from its 32-byte seed. A nil seed
// disables minting.
func (s *TokenSigner) SetSeed(seed []byte) {
	s.prev.Store(nil)
	if len(seed) != ed25519.SeedSize {
		s.key.Store(nil)
		return
	}
	priv := ed25519.NewKeyFromSeed(seed)
	s.key.Store(&signingKey{priv: priv, kid: crypto.KeyID(priv.Public().(ed25519.PublicKey))})
}

// PublicKey returns the verification half, or nil when no key is installed.
func (s *TokenSigner) PublicKey() ed25519.PublicKey {
	k := s.key.Load()
	if k == nil {
		return nil
	}
	return k.priv.Public().(ed25519.PublicKey)
}

// SetPreviousPublicKey records the key a rotation retired. Call it after
// SetSeed, which clears it.
func (s *TokenSigner) SetPreviousPublicKey(pub ed25519.PublicKey) {
	if len(pub) == 0 {
		s.prev.Store(nil)
		return
	}
	s.prev.Store(&pub)
}

// verificationKeys are the keys a live token may have been signed with.
func (s *TokenSigner) verificationKeys() []ed25519.PublicKey {
	var out []ed25519.PublicKey
	if pub := s.PublicKey(); pub != nil {
		out = append(out, pub)
	}
	if prev := s.prev.Load(); prev != nil {
		out = append(out, *prev)
	}
	return out
}

// ErrNoSigningKey means the deployment has no token signing key — tokens
// cannot be minted until one is configured.
var ErrNoSigningKey = errors.New("control: no token signing key")

func (s *TokenSigner) sign(claims crypto.TokenClaims) (string, error) {
	k := s.key.Load()
	if k == nil {
		return "", ErrNoSigningKey
	}
	return crypto.SignToken(k.priv, k.kid, claims)
}

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

// MintLimiter meters how often one user may mint a token. Satisfied by
// *pkg/ratelimit.Limiter; nil leaves minting unmetered.
type MintLimiter interface {
	Reserve(ctx context.Context, scope string, rules []pkgratelimit.Rule) (*pkgratelimit.Reservation, error)
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

// rotateTokenKey re-keys the signer. Deployment-wide and unscoped, so it is
// gated on settings.update rather than a tenant verb.
func rotateTokenKey(ctx context.Context, d Deps) (*emptyOutput, error) {
	if err := d.Authz.Authorize(ctx, "settings.update", authz.Resource{Kind: "settings"}); err != nil {
		return nil, mapAuthzErr(err)
	}
	if d.RotateTokenKey == nil {
		return nil, huma.Error503ServiceUnavailable("tokens_disabled: no signing-key store is configured")
	}
	if err := d.RotateTokenKey(ctx); err != nil {
		return nil, huma.Error500InternalServerError("signing-key rotation failed: " + err.Error())
	}
	audit.Record(ctx, "tokens.rotate", audit.Resource{Kind: "token", Name: "signing-key"}, audit.StatusAllowed)
	return &emptyOutput{}, nil
}

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

// reserveMint meters one mint against the caller's fixed window. A limiter
// outage must not take minting down, so only an explicit budget violation
// is fatal.
func reserveMint(ctx context.Context, d tokenDeps, userID string) error {
	if d.limiter == nil {
		return nil
	}
	_, err := d.limiter.Reserve(ctx, mintLimitScope(userID), []pkgratelimit.Rule{{
		Key:      mintLimitRule,
		Name:     "token mints",
		Meter:    "requests",
		Strategy: pkgratelimit.StrategyFixedWindow,
		Amount:   mintLimitBudget,
		Window:   mintLimitWindow,
	}})
	if errors.Is(err, pkgratelimit.ErrExceeded) {
		return huma.Error429TooManyRequests("too many token mints; retry shortly")
	}
	return nil
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
		if claims, err = crypto.ParseToken(pub, raw); err == nil {
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
