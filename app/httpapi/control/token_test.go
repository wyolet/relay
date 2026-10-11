package control

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	authtoken "github.com/wyolet/relay/auth/token"
	"github.com/wyolet/relay/pkg/crypto"
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

// memDenylist records the revocation entries a revoke writes.
type memDenylist struct {
	mu      sync.Mutex
	entries map[string]time.Duration
}

func (d *memDenylist) Set(_ context.Context, key string, _ []byte, ttl time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = map[string]time.Duration{}
	}
	d.entries[key] = ttl
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
	if len(ev.Resource.Scope) != 2 || !strings.HasPrefix(ev.Resource.Scope[0], "project:") ||
		!strings.HasPrefix(ev.Resource.Scope[1], "team:") {
		t.Errorf("audit scope = %v, want project then team", ev.Resource.Scope)
	}
}

func TestMintToken_Rejections(t *testing.T) {
	f := newTokenFixture(t)
	userActor := &actor.Actor{UserID: f.userID, Username: "alice"}

	for _, tc := range []struct {
		name       string
		actor      *actor.Actor
		body       *mintTokenInput
		deps       func(tokenDeps) tokenDeps
		wantStatus int
	}{
		{
			name: "ttl beyond the maximum", actor: userActor,
			body: mintBody(f.project.Meta.Name, "48h"), wantStatus: http.StatusBadRequest,
		},
		{
			name: "unparseable ttl", actor: userActor,
			body: mintBody(f.project.Meta.Name, "soon"), wantStatus: http.StatusBadRequest,
		},
		{
			name: "unknown project", actor: userActor,
			body: mintBody("no-such-project", ""), wantStatus: http.StatusNotFound,
		},
		{
			name: "admin token has no user to name", actor: &actor.Actor{AdminToken: true},
			body: mintBody(f.project.Meta.Name, ""), wantStatus: http.StatusBadRequest,
		},
		{
			name: "tokens disabled", actor: userActor, body: mintBody(f.project.Meta.Name, ""),
			deps: func(d tokenDeps) tokenDeps {
				d.cfg = func() *settings.AuthTokens { return &settings.AuthTokens{} }
				return d
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := f.deps
			if tc.deps != nil {
				deps = tc.deps(deps)
			}
			_, err := mintToken(actor.WithActor(context.Background(), tc.actor), deps, tc.body)
			if status := statusOf(t, err); status != tc.wantStatus {
				t.Fatalf("status = %d (%v), want %d", status, err, tc.wantStatus)
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
	want := policy.RevokedKey(f.team.Meta.ID, minted.Body.JTI)
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
	if _, ok := f.denylist.entries[policy.RevokedKey(f.team.Meta.ID, minted.Body.JTI)]; !ok {
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

func TestMintToken_ResponseCarriesProjectTeamID(t *testing.T) {
	f := newTokenFixture(t)
	a := &actor.Actor{UserID: f.userID, Username: "alice"}

	out, err := mintToken(actor.WithActor(context.Background(), a), f.deps, mintBody(f.project.Meta.Name, ""))
	if err != nil {
		t.Fatalf("mintToken: %v", err)
	}
	if out.Body.TeamID != f.team.Meta.ID {
		t.Errorf("teamId = %q, want %q", out.Body.TeamID, f.team.Meta.ID)
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
		want := policy.RevokedKey(teamHint, minted.Body.JTI)
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
	if _, ok := f.denylist.entries[policy.RevokedKey(f.team.Meta.ID, minted.Body.JTI)]; !ok {
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
