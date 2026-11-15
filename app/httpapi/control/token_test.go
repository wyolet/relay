package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/meta"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// tokenUsers is the in-memory user store the token handlers read versions
// from and bump.
type tokenUsers map[string]*user.User

func (u tokenUsers) Get(_ context.Context, id string) (*user.User, error) { return u[id], nil }
func (u tokenUsers) BumpTokenVersion(_ context.Context, id string) error {
	if u[id] == nil {
		return errors.New("no such user")
	}
	u[id].TokenVersion++
	return nil
}

// memDenylist records each revoked jti with the time left until its token expires.
type memDenylist struct {
	mu      sync.Mutex
	entries map[string]time.Duration
}

func (d *memDenylist) Revoke(_ context.Context, jti string, exp time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = map[string]time.Duration{}
	}
	d.entries[jti] = time.Until(exp)
	return nil
}

// staticAudit answers the mint lookup revoke-by-jti performs.
type staticAudit []audit.Event

func (s staticAudit) Events(_ context.Context, q audit.Query) ([]audit.Event, error) {
	var out []audit.Event
	for _, ev := range s {
		if q.ResourceID != "" && ev.Resource.ID != q.ResourceID {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

type tokenFixture struct {
	deps     tokenDeps
	team     *team.Team
	project  *project.Project
	pol      *policy.Policy
	users    tokenUsers
	denylist *memDenylist
	userID   string
	signer   *TokenSigner
}

func newTokenFixture(t *testing.T, audits ...audit.Event) tokenFixture {
	t.Helper()
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-pol", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}}}
	userID := meta.NewID()
	g := &group.Group{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "data-science", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: group.Spec{MemberIDs: []string{userID}},
	}

	cat := catalogtest.Catalog{
		Policies: []*policy.Policy{pol},
		Teams:    []*team.Team{tm},
		Projects: []*project.Project{proj},
		Groups:   []*group.Group{g},
	}.Load(t)

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	signer := &TokenSigner{}
	signer.SetSeed(seed)

	users := tokenUsers{userID: {ID: userID, Username: "alice", TokenVersion: 3}}
	deny := &memDenylist{}
	return tokenFixture{
		deps: tokenDeps{
			cfg: func() *settings.AuthTokens {
				return &settings.AuthTokens{Enabled: true, DefaultTTL: time.Hour, MaxTTL: 24 * time.Hour}
			},
			snap:     cat.Current,
			signer:   signer,
			denylist: deny,
			users:    users,
			audit:    staticAudit(audits),
			authz:    authz.AlwaysAllowAuthenticated{},
		},
		team: tm, project: proj, pol: pol,
		users: users, denylist: deny, userID: userID, signer: signer,
	}
}

// audited runs fn inside the audit middleware so audit.Record has somewhere
// to land, and returns the rows it produced.
func audited(t *testing.T, a *actor.Actor, fn func(ctx context.Context)) []audit.Event {
	t.Helper()
	sink := &auditSink{}
	em := audit.NewEmitter(sink, slog.New(slog.DiscardHandler))
	h := audit.Middleware(em, nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		fn(actor.WithActor(r.Context(), a))
	}))
	r := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)
	em.Close()
	return sink.all()
}

func TestMintToken_Claims(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice", IdPGroups: []string{"platform-eng"}}

	var out *mintTokenOutput
	events := audited(t, a, func(ctx context.Context) {
		var err error
		out, err = mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, "30m"))
		if err != nil {
			t.Fatalf("mintToken: %v", err)
		}
	})

	claims, err := authtoken.Parse[crypto.TokenClaims](f.signer.PublicKey(), out.Body.Token)
	if err != nil {
		t.Fatalf("the minted token does not verify: %v", err)
	}
	if claims.Iss != crypto.TokenIssuer || claims.Sub != "user:"+f.userID || claims.Prj != f.project.Meta.ID {
		t.Errorf("claims = %+v", claims)
	}
	if claims.Ver != 3 {
		t.Errorf("ver = %d, want the user's current token version", claims.Ver)
	}
	if claims.Jti != out.Body.JTI || claims.Exp != out.Body.ExpiresAt.Unix() {
		t.Errorf("claims (%q, %d) disagree with the response (%q, %d)",
			claims.Jti, claims.Exp, out.Body.JTI, out.Body.ExpiresAt.Unix())
	}
	if d := time.Until(out.Body.ExpiresAt); d > 30*time.Minute || d < 29*time.Minute {
		t.Errorf("expiry in %v, want the requested 30m", d)
	}
	// Only the IdP's groups ride the claim: local membership (data-science) is read from the snapshot at every verification, so a removal is not held until expiry.
	if len(claims.Grp) != 1 || claims.Grp[0] != "platform-eng" {
		t.Errorf("grp = %v, want only the IdP groups", claims.Grp)
	}

	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.Action != "tokens.mint" || ev.Resource.ID != claims.Jti {
		t.Errorf("audit row = %+v, want a tokens.mint naming the jti", ev)
	}
	if ev.Resource.Name != out.Body.ExpiresAt.Format(time.RFC3339) {
		t.Errorf("audit row carries expiry %q, want the token's exp", ev.Resource.Name)
	}
	if want := []string{"project:" + f.project.Meta.ID, "team:" + f.team.Meta.ID}; !slices.Equal(ev.Resource.Scope, want) {
		t.Errorf("audit scope = %v, want %v", ev.Resource.Scope, want)
	}
	if out.Body.TeamID != f.team.Meta.ID {
		t.Errorf("teamId = %q, want the project's team %q", out.Body.TeamID, f.team.Meta.ID)
	}
}

// A refused mint signs nothing and says why; a project the caller may not mint for answers exactly as one that does not exist, so the answer enumerates no slugs.
func TestMintToken_Rejections(t *testing.T) {
	f := newTokenFixture(t)
	f.users["u-disabled"] = &user.User{ID: "u-disabled", Username: "mallory", Disabled: true}
	alice := &actor.Actor{UserID: f.userID, Username: "alice"}
	off := func(d *tokenDeps) { d.cfg = func() *settings.AuthTokens { return &settings.AuthTokens{} } }

	for _, tc := range []struct {
		name         string
		actor        *actor.Actor
		project, ttl string
		deps         func(*tokenDeps)
		status       int
		code, msg    string
	}{
		{"ttl beyond the maximum", alice, "ml-search", "48h", nil, 400, "", "ttl exceeds the configured maximum of 24h0m0s"},
		{"ttl just past the maximum", alice, "ml-search", "24h0m1s", nil, 400, "", "ttl exceeds the configured maximum of 24h0m0s"},
		{"unparseable ttl", alice, "ml-search", "soon", nil, 400, "", "ttl must be a positive Go duration"},
		{"zero ttl", alice, "ml-search", "0s", nil, 400, "", "ttl must be a positive Go duration"},
		{"negative ttl", alice, "ml-search", "-5m", nil, 400, "", "ttl must be a positive Go duration"},
		{"unknown project", alice, "no-such-project", "", nil, 404, "", "project not found"},
		{"a project the caller may not mint for", alice, "ml-search", "", func(d *tokenDeps) { d.authz = denyAll{} }, 404, "", "project not found"},
		{"the admin token has no user to name", &actor.Actor{AdminToken: true}, "ml-search", "", nil, 400, "", "minting a token requires a user session"},
		{"tokens disabled", alice, "ml-search", "", off, 503, "", "tokens_disabled: no token signing key is configured"},
		{"a user with no account row", &actor.Actor{UserID: "u-gone"}, "ml-search", "", nil, 404, "", "user not found"},
		{"a disabled account", &actor.Actor{UserID: "u-disabled"}, "ml-search", "", nil, 401, "account_disabled", "account is disabled; contact your administrator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := f.deps
			if tc.deps != nil {
				tc.deps(&deps)
			}
			events := audited(t, tc.actor, func(ctx context.Context) {
				out, err := mintToken(ctx, deps, mintBody(tc.project, tc.ttl))
				code := ""
				if ae, ok := err.(*httpapi.APIError); ok {
					code = ae.Err.Code
				}
				if out != nil || statusOf(t, err) != tc.status || code != tc.code || err.Error() != tc.msg {
					t.Errorf("mint = %+v, %d %q %v; want %d %q %q", out, statusOf(t, err), code, err, tc.status, tc.code, tc.msg)
				}
			})
			if len(events) != 0 {
				t.Errorf("a refused mint recorded %+v", events)
			}
		})
	}
}

func TestRevokeToken_ByJTI(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice"}

	var minted *mintTokenOutput
	audited(t, a, func(ctx context.Context) {
		var err error
		minted, err = mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
		if err != nil {
			t.Fatalf("mintToken: %v", err)
		}
	})

	// The revoke path reads the mint's own audit row for the project and
	// expiry; nothing else records a token.
	f.deps.audit = staticAudit{{
		Actor:  audit.Actor{Kind: audit.ActorUser, ID: f.userID},
		Action: "tokens.mint",
		Resource: audit.Resource{
			Kind: "token", ID: minted.Body.JTI,
			Name:  minted.Body.ExpiresAt.Format(time.RFC3339),
			Owner: &meta.Owner{Kind: meta.OwnerProject, ID: f.project.Meta.ID},
		},
	}}

	if _, err := revokeTokenByJTI(actor.WithActor(context.Background(), a), f.deps, minted.Body.JTI, "", ""); err != nil {
		t.Fatalf("revokeTokenByJTI: %v", err)
	}
	want := minted.Body.JTI
	ttl, ok := f.denylist.entries[want]
	if !ok {
		t.Fatalf("denylist entries = %v, want one at %q", f.denylist.entries, want)
	}
	if ttl <= 0 || ttl > time.Hour {
		t.Errorf("denylist ttl = %v, want the token's remaining life", ttl)
	}

	// A jti nothing minted is a 404, not a silent success.
	if _, err := revokeTokenByJTI(actor.WithActor(context.Background(), a), f.deps, meta.NewID(), "", ""); statusOf(t, err) != http.StatusNotFound {
		t.Errorf("unknown jti: err = %v, want 404", err)
	}
}

func TestRevokeToken_ByValue(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice"}

	var minted *mintTokenOutput
	audited(t, a, func(ctx context.Context) {
		var err error
		minted, err = mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
		if err != nil {
			t.Fatalf("mintToken: %v", err)
		}
	})

	// No session, no audit lookup: the token itself carries the project.
	if _, err := revokeTokenByValue(context.Background(), f.deps, minted.Body.Token); err != nil {
		t.Fatalf("revokeTokenByValue: %v", err)
	}
	if _, ok := f.denylist.entries[minted.Body.JTI]; !ok {
		t.Fatalf("denylist entries = %v, want the minted jti", f.denylist.entries)
	}
	if _, err := revokeTokenByValue(context.Background(), f.deps, "eyJ.garbage.token"); statusOf(t, err) != http.StatusUnauthorized {
		t.Errorf("garbage token: err = %v, want 401", err)
	}
}

func TestRevokeAll_BumpsTokenVersion(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice"}

	events := audited(t, a, func(ctx context.Context) {
		if _, err := bumpTokenVersion(ctx, f.deps, f.userID); err != nil {
			t.Fatalf("bumpTokenVersion: %v", err)
		}
	})
	if got := f.users[f.userID].TokenVersion; got != 4 {
		t.Errorf("token version = %d, want 4", got)
	}
	if len(events) != 1 || events[0].Action != "tokens.revoke-all" {
		t.Errorf("audit events = %+v, want one tokens.revoke-all", events)
	}
	// Every token minted before the bump now carries a stale ver, which the
	// data plane rejects — see the inference token tests.
	if _, err := bumpTokenVersion(context.Background(), f.deps, meta.NewID()); statusOf(t, err) != http.StatusNotFound {
		t.Errorf("unknown user: err = %v, want 404", err)
	}
}

func TestMintToken_ClampsMisconfiguredDefaultTTLToMax(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice"}
	f.deps.cfg = func() *settings.AuthTokens {
		return &settings.AuthTokens{Enabled: true, DefaultTTL: 48 * time.Hour, MaxTTL: 24 * time.Hour}
	}

	out, err := mintToken(actor.WithActor(context.Background(), a), f.deps, mintBody(f.project.Meta.Name, ""))
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	if d := time.Until(out.Body.ExpiresAt); d > 24*time.Hour || d < 23*time.Hour {
		t.Errorf("expiry in %v, want clamped to the 24h max, not the misconfigured 48h default", d)
	}
}

func TestRevokeToken_ByJTI_HintsPathRequiresAdminAndNoAuditRow(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice"}

	var minted *mintTokenOutput
	audited(t, a, func(ctx context.Context) {
		var err error
		minted, err = mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
		if err != nil {
			t.Fatalf("mintToken: %v", err)
		}
	})
	// No audit row recorded for this jti — the lookup finds nothing, so the
	// hints are the only way to revoke.
	f.deps.audit = staticAudit{}
	teamHint := minted.Body.TeamID
	expHint := minted.Body.ExpiresAt.Format(time.RFC3339)

	t.Run("no hints and no audit row is 404", func(t *testing.T) {
		admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
		if _, err := revokeTokenByJTI(admin, f.deps, minted.Body.JTI, "", ""); statusOf(t, err) != http.StatusNotFound {
			t.Errorf("err = %v, want 404", err)
		}
	})

	t.Run("non-admin caller with hints is 403", func(t *testing.T) {
		nonAdmin := actor.WithActor(context.Background(), a)
		if _, err := revokeTokenByJTI(nonAdmin, f.deps, minted.Body.JTI, teamHint, expHint); statusOf(t, err) != http.StatusForbidden {
			t.Errorf("err = %v, want 403", err)
		}
	})

	t.Run("admin caller with hints revokes and writes the denylist entry", func(t *testing.T) {
		admin := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
		if _, err := revokeTokenByJTI(admin, f.deps, minted.Body.JTI, teamHint, expHint); err != nil {
			t.Fatalf("revokeTokenByJTI: %v", err)
		}
		want := minted.Body.JTI
		ttl, ok := f.denylist.entries[want]
		if !ok {
			t.Fatalf("denylist entries = %v, want one at %q", f.denylist.entries, want)
		}
		if ttl <= 0 || ttl > time.Hour {
			t.Errorf("denylist ttl = %v, want the token's remaining life", ttl)
		}
	})
}

func mintBody(project, ttl string) *mintTokenInput {
	in := &mintTokenInput{}
	in.Body.Project = project
	in.Body.TTL = ttl
	return in
}

// A rotation must not strand the tokens minted before it: revoke-by-value
// verifies against the retired key as well as the live one.
func TestTokenRevokeByValuePreviousKey(t *testing.T) {
	f := newTokenFixture(t)
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: f.userID, Username: "alice"})

	minted, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, "30m"))
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	retired := f.signer.PublicKey()

	next := make([]byte, ed25519.SeedSize)
	for i := range next {
		next[i] = byte(200 - i)
	}
	f.signer.SetSeed(next)
	f.signer.SetPreviousPublicKey(retired)

	if _, err := revokeTokenByValue(ctx, f.deps, minted.Body.Token); err != nil {
		t.Fatalf("revokeTokenByValue after a rotation: %v", err)
	}
	f.denylist.mu.Lock()
	defer f.denylist.mu.Unlock()
	if _, ok := f.denylist.entries[minted.Body.JTI]; !ok {
		t.Fatalf("no denylist entry written; entries = %v", f.denylist.entries)
	}
}

// The generated OpenAPI is what a client codegen believes: a status the
// handler can return but the operation does not declare is a response the
// client has no type for.
func TestTokenOperationsDeclareTheirErrorStatuses(t *testing.T) {
	api := humachi.New(chi.NewRouter(), huma.DefaultConfig("token-errors-test", "0"))
	registerTokens(api, Deps{Authz: authz.AlwaysAllowAuthenticated{}}, nil)
	paths := api.OpenAPI().Paths

	for _, tc := range []struct {
		path  string
		codes []int
	}{
		{"/auth/token", []int{429, 500}},
		{"/auth/token/revoke", []int{400, 500}},
	} {
		item := paths[tc.path]
		if item == nil || item.Post == nil {
			t.Fatalf("no POST operation at %s", tc.path)
		}
		for _, code := range tc.codes {
			if _, ok := item.Post.Responses[strconv.Itoa(code)]; !ok {
				t.Errorf("POST %s does not declare %d", tc.path, code)
			}
		}
	}
}

// denyAll refuses everything an authenticated caller asks for.
type denyAll struct{}

func (denyAll) Authorize(context.Context, string, authz.Resource) error {
	return authz.ErrForbidden
}

// Minting signs a long-lived credential off a session; the window is a floor
// under abuse, not a quota.
func TestMintIsRateLimitedPerUser(t *testing.T) {
	f := newTokenFixture(t)
	store := kv.NewMem()
	t.Cleanup(func() { _ = store.Close() })
	f.deps.limiter = pkgratelimit.New(store, slog.Default(), nil)
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: f.userID, Username: "alice"})

	for i := 0; i < mintLimitBudget; i++ {
		if _, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, "")); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	_, err := mintToken(ctx, f.deps, mintBody(f.project.Meta.Name, ""))
	if got := statusOf(t, err); got != 429 {
		t.Fatalf("status past the budget = %d, want 429", got)
	}

	// The window is per user: another account is unaffected.
	other := "u-other"
	f.users[other] = f.users[f.userID]
	otherCtx := actor.WithActor(context.Background(), &actor.Actor{UserID: other, Username: "bob"})
	if _, err := mintToken(otherCtx, f.deps, mintBody(f.project.Meta.Name, "")); err != nil {
		t.Fatalf("another user must have their own window: %v", err)
	}

	entries, err := store.Range(context.Background(), mintLimitKeyPrefix(f.userID))
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(entries) == 0 {
		t.Errorf("no kv key under %q", mintLimitKeyPrefix(f.userID))
	}
}

// The hash tag keeps one user's window on one Redis Cluster slot.
func TestMintLimitKeyFormat(t *testing.T) {
	if got, want := mintLimitScope("u-1"), "user:u-1"; got != want {
		t.Errorf("scope = %q, want %q", got, want)
	}
	if got, want := mintLimitKeyPrefix("u-1"), "limit:{user:u-1}:fw:"+mintLimitRule+":requests:"; got != want {
		t.Errorf("key prefix = %q, want %q", got, want)
	}
}

func asAdminToken() context.Context {
	return actor.WithActor(context.Background(), &actor.Actor{AdminToken: true, Username: "admin-token"})
}

// Rotation is an admin's, runs the wired rotation once, and says so when there is no store to keep a new key in or the rotation fails.
func TestRotateTokenKey(t *testing.T) {
	admin := asAdminToken()
	for _, tc := range []struct {
		name   string
		authz  authz.Authorizer
		wired  bool
		err    error
		status int
		msg    string
		runs   int
	}{
		{"rotates", authz.AlwaysAllowAuthenticated{}, true, nil, 0, "", 1},
		{"a denied caller rotates nothing", denyAll{}, true, nil, 403, "forbidden", 0},
		{"no secret store", authz.AlwaysAllowAuthenticated{}, false, nil, 503, "tokens_disabled: no signing-key store is configured", 0},
		{"the rotation fails", authz.AlwaysAllowAuthenticated{}, true, errors.New("no master key"), 500, "signing-key rotation failed: no master key", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runs := 0
			d := Deps{Authz: tc.authz}
			if tc.wired {
				d.RotateTokenKey = func(context.Context) error { runs++; return tc.err }
			}
			_, err := rotateTokenKey(admin, d)
			if statusOf(t, err) != tc.status || (err != nil && err.Error() != tc.msg) || runs != tc.runs {
				t.Fatalf("rotate = %d %v after %d runs, want %d %q after %d", statusOf(t, err), err, runs, tc.status, tc.msg, tc.runs)
			}
		})
	}
}
