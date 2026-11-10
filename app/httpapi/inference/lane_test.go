package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/meta"
)

// laneStack is the principal stack behind LaneMiddleware, recording the path the routes saw.
type laneStack struct {
	*principalStack
	handler http.Handler
	path    string
}

// laneFixture is the principal fixture whose user also holds a member account in the team that takes keys (teamMember) and one in the project that, made on first use, serves tokens only (f.memberAcct).
type laneFixture struct {
	principalFixture
	teamMember, otherPersonal *account.Account
	key                       *key.Key
}

func newLaneFixture() laneFixture {
	f := newPrincipalFixture()
	tm := account.Member(f.user, "u-"+f.user, meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}, f.team.Meta.Name, "")
	tm.Meta.ID, tm.Meta.Name, tm.FollowsRoleBindings = meta.NewID(), "acme-ml", true
	other := meta.NewID()
	f.versions[other] = 1
	op := account.Personal(other, "u-"+other)
	op.Meta.ID = meta.NewID()
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "alice", Owner: meta.Owner{Kind: meta.OwnerUser, ID: f.user}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user}, KeyHash: sha("sk-wr-alice"), AccountID: f.personalAcct.Meta.ID},
	}
	return laneFixture{principalFixture: f, teamMember: tm, otherPersonal: op, key: k}
}

func (f laneFixture) stack(t *testing.T, domain string) *laneStack {
	t.Helper()
	f.extraAccounts = []*account.Account{f.teamMember, f.otherPersonal}
	st := &laneStack{principalStack: f.principalFixture.stack(t, f.key)}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.seen, st.path = PrincipalFrom(r.Context()), r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	st.handler = LaneMiddleware(domain)(ClassifyMiddleware()(PrincipalMiddleware(st.auth)(inner)))
	return st
}

func (st *laneStack) send(bearer, path string, set func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	if set != nil {
		set(r)
	}
	st.seen, st.path = nil, ""
	w := httptest.NewRecorder()
	st.handler.ServeHTTP(w, r)
	return w
}

func accountHeader(v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(httpheader.HeaderAccount, v) }
}

func atHost(v string) func(*http.Request) { return func(r *http.Request) { r.Host = v } }

func (st *laneStack) wantLane(t *testing.T, w *httptest.ResponseRecorder, want *account.Account) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if got := st.seen.Caller.Lane.Meta.ID; got != want.Meta.ID {
		t.Fatalf("lane = %s, want %s", st.seen.Caller.Lane.Meta.Name, want.Meta.Name)
	}
	if st.path != "/v1/chat/completions" {
		t.Fatalf("route saw path %q", st.path)
	}
}

func wantForbidden(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body)
	}
	var body struct {
		Error struct{ Code, Refusal string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	if body.Error.Code != code {
		t.Fatalf("code = %q, want %q", body.Error.Code, code)
	}
}

// A request switches its lane by header, by /a/{account}/ base URL or by per-account host, naming the account by id or name.
func TestLane_SwitchByHeaderPathAndHost(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "acc.example.com")

	w := st.send("sk-wr-alice", "/v1/chat/completions", nil)
	st.wantLane(t, w, f.personalAcct)
	if st.seen.SwitchedLaneID != "" {
		t.Fatalf("default lane recorded a switch to %s", st.seen.SwitchedLaneID)
	}

	for name, req := range map[string]struct {
		path string
		set  func(*http.Request)
	}{
		"header by id":   {"/v1/chat/completions", accountHeader(f.teamMember.Meta.ID)},
		"header by name": {"/v1/chat/completions", accountHeader(f.teamMember.Meta.Name)},
		"path":           {"/a/acme-ml/v1/chat/completions", nil},
		"path by id":     {"/a/" + f.teamMember.Meta.ID + "/v1/chat/completions", nil},
		"host":           {"/v1/chat/completions", atHost("acme-ml.acc.example.com")},
		"host with port": {"/v1/chat/completions", atHost("ACME-ML.Acc.Example.com:8080")},
	} {
		t.Run(name, func(t *testing.T) {
			w := st.send("sk-wr-alice", req.path, req.set)
			st.wantLane(t, w, f.teamMember)
			if st.seen.SwitchedLaneID != f.teamMember.Meta.ID || st.seen.TeamID != f.team.Meta.ID {
				t.Fatalf("switched = %q team = %q", st.seen.SwitchedLaneID, st.seen.TeamID)
			}
		})
	}
}

// The header wins over the path, the path over the host; a host outside the domain, or any host with the domain unset, names nothing.
func TestLane_Precedence(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "acc.example.com")
	personal := f.personalAcct.Meta.Name

	st.wantLane(t, st.send("sk-wr-alice", "/a/acme-ml/v1/chat/completions", accountHeader(personal)), f.personalAcct)
	st.wantLane(t, st.send("sk-wr-alice", "/a/"+personal+"/v1/chat/completions", atHost("acme-ml.acc.example.com")), f.personalAcct)
	st.wantLane(t, st.send("sk-wr-alice", "/v1/chat/completions", atHost("acme-ml.other.example.com")), f.personalAcct)
	st.wantLane(t, st.send("sk-wr-alice", "/v1/chat/completions", atHost("x.acme-ml.acc.example.com")), f.personalAcct)

	off := f.stack(t, "")
	off.wantLane(t, off.send("sk-wr-alice", "/v1/chat/completions", atHost("acme-ml.acc.example.com")), f.personalAcct)
}

// A key switches only into an account it could be saved in: another user's, a team's own, a token-only member account and one that does not exist are refused alike with account_not_held; a closed one with account_closed.
func TestLane_KeyRefusals(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "")
	for name, ref := range map[string]string{
		"another user's":    f.otherPersonal.Meta.ID,
		"the team's own":    f.teamAcct.Meta.ID,
		"token-only member": f.memberAcct.Meta.Name,
		"unknown":           "no-such-account",
	} {
		t.Run(name, func(t *testing.T) {
			w := st.send("sk-wr-alice", "/v1/chat/completions", accountHeader(ref))
			wantForbidden(t, w, plan.CodeAccountNotHeld)
			if st.seen != nil {
				t.Fatal("a refused switch reached the route")
			}
		})
	}
	t.Run("closed", func(t *testing.T) {
		closed := *f.teamMember
		closed.Spec.Closed = true
		g := f
		g.teamMember = &closed
		cst := g.stack(t, "")
		wantForbidden(t, cst.send("sk-wr-alice", "/a/acme-ml/v1/chat/completions", nil), plan.CodeAccountClosed)
	})
}

// A relay-minted token switches by the same means. One scoped to a project stays inside it: its member account there, made on first use, and nothing outside. One without a project switches into any account its user holds, and is refused one it does not hold.
func TestLane_TokenSwitch(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "")
	tok := f.mint(t, nil)

	st.wantLane(t, st.send(tok, "/v1/chat/completions", nil), f.memberAcct)
	st.wantLane(t, st.send(tok, "/v1/chat/completions", accountHeader(f.memberAcct.Meta.ID)), f.memberAcct)
	wantForbidden(t, st.send(tok, "/a/acme-ml/v1/chat/completions", nil), plan.CodeAccountNotHeld)
	wantForbidden(t, st.send(tok, "/v1/chat/completions", accountHeader(f.personalAcct.Meta.ID)), plan.CodeAccountNotHeld)

	unscoped := f.mint(t, func(c *crypto.TokenClaims) { c.Prj = "" })
	st.wantLane(t, st.send(unscoped, "/a/acme-ml/v1/chat/completions", nil), f.teamMember)
	st.wantLane(t, st.send(unscoped, "/v1/chat/completions", accountHeader(f.personalAcct.Meta.ID)), f.personalAcct)
	if st.seen.SwitchedLaneID != f.personalAcct.Meta.ID || st.seen.ProjectID != "" {
		t.Fatalf("switched = %q project = %q", st.seen.SwitchedLaneID, st.seen.ProjectID)
	}
	wantForbidden(t, st.send(unscoped, "/v1/chat/completions", accountHeader(f.otherPersonal.Meta.ID)), plan.CodeAccountNotHeld)
}

// recordUsage gives st's credential middleware a usage hook whose rows land in the returned capture.
func (st *laneStack) recordUsage() usageCapture {
	events := make(usageCapture, 16)
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterCollector(events)
	st.auth.Lifecycle = reg
	return events
}

// A request refused at the credential check, once its credential is known, leaves a usage row with the refusal; a bad credential leaves none.
func TestLane_EdgeRefusalsWriteAUsageRow(t *testing.T) {
	f := newLaneFixture()
	closed := *f.teamMember
	closed.Spec.Closed = true
	g := f
	g.teamMember = &closed
	st, cst := f.stack(t, ""), g.stack(t, "")
	events := st.recordUsage()
	cst.auth.Lifecycle = st.auth.Lifecycle

	if w := st.send("sk-wr-nobody", "/a/acme-ml/v1/chat/completions", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key = %d", w.Code)
	}
	tok := f.mint(t, nil)
	for _, tc := range []struct {
		name                                 string
		st                                   *laneStack
		bearer, path, code, lane, credential string
	}{
		{"key into an account it does not hold", st, "sk-wr-alice", "/a/" + f.otherPersonal.Meta.ID + "/v1/chat/completions", plan.CodeAccountNotHeld, "", f.key.Meta.ID},
		{"key into a closed account", cst, "sk-wr-alice", "/a/acme-ml/v1/chat/completions", plan.CodeAccountClosed, closed.Meta.ID, f.key.Meta.ID},
		{"project token outside its project", st, tok, "/a/acme-ml/v1/chat/completions", plan.CodeAccountNotHeld, "", ""},
	} {
		wantForbidden(t, tc.st.send(tc.bearer, tc.path, nil), tc.code)
		ev := events.next(t)
		if ev.ErrorKind != tc.code || ev.Refusal != string(plan.RefusalModelNotAllowed) || ev.Status != http.StatusForbidden {
			t.Errorf("%s: row error_kind %q refusal %q status %d", tc.name, ev.ErrorKind, ev.Refusal, ev.Status)
		}
		if ev.PrincipalID != f.user || ev.LaneAccountID != tc.lane || (tc.credential != "" && ev.CredentialID != tc.credential) {
			t.Errorf("%s: row principal %q lane %q credential %q", tc.name, ev.PrincipalID, ev.LaneAccountID, ev.CredentialID)
		}
	}
}

// A principal switched off answers 401 as before, and its row records why.
func TestLane_DisabledSubjectWritesAUsageRow(t *testing.T) {
	f := newPrincipalFixture()
	fls := false
	disabled := *f.sa
	disabled.Spec.Enabled = &fls
	f.sa = &disabled
	ps := f.stack(t, saKey(f, "sk-wr-live"))
	st := &laneStack{principalStack: ps, handler: ClassifyMiddleware()(PrincipalMiddleware(ps.auth)(http.NotFoundHandler()))}
	events := st.recordUsage()
	if w := st.send("sk-wr-live", "/v1/chat/completions", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if ev := events.next(t); ev.ErrorKind != plan.CodeSubjectDisabled || ev.Refusal != string(plan.RefusalModelNotAllowed) || ev.PrincipalID != f.sa.Meta.ID ||
		ev.Status != http.StatusUnauthorized || ev.ErrorMessage != "api key disabled" {
		t.Fatalf("row error_kind %q refusal %q principal %q status %d message %q", ev.ErrorKind, ev.Refusal, ev.PrincipalID, ev.Status, ev.ErrorMessage)
	}
}

// The lifecycle, and so the usage event, records the lane the request ran in: the default, or the one it switched into.
func TestLane_StampedOnTheLifecycle(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "")
	for path, want := range map[string]*account.Account{
		"/v1/chat/completions":           f.personalAcct,
		"/a/acme-ml/v1/chat/completions": f.teamMember,
	} {
		st.wantLane(t, st.send("sk-wr-alice", path, nil), want)
		lc := lifecycle.NewContext("r", "pipeline", time.Now())
		applyPrincipalIdentity(lc, st.seen)
		if lc.LaneAccountID != want.Meta.ID {
			t.Errorf("%s: lane_account_id = %q, want %s", path, lc.LaneAccountID, want.Meta.Name)
		}
	}
}

// A switched WebSocket connection re-resolves each frame in the lane it switched into.
func TestLane_ReauthenticateKeepsTheSwitch(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "")
	st.wantLane(t, st.send("sk-wr-alice", "/a/acme-ml/v1/chat/completions", nil), f.teamMember)
	fp, ok := st.auth.reauthenticate(httptest.NewRecorder(), nil, st.seen)
	if !ok || fp.Caller.Lane.Meta.ID != f.teamMember.Meta.ID {
		t.Fatalf("frame lane = %v, ok %v", fp, ok)
	}
}

// A queued batch item runs in the lane its submission switched into, and is refused once its credential may no longer use it.
func TestLane_RecheckRunsInTheSwitchedLane(t *testing.T) {
	f := newLaneFixture()
	st := f.stack(t, "")
	now := time.Now()
	p, err := st.auth.Recheck(sha("sk-wr-alice"), "", "", "", 0, 0, f.teamMember.Meta.ID, now)
	if err != nil || p.Caller.Lane.Meta.ID != f.teamMember.Meta.ID || p.SwitchedLaneID != f.teamMember.Meta.ID {
		t.Fatalf("key item lane = %v, %v", p, err)
	}
	if _, err := st.auth.Recheck(sha("sk-wr-alice"), "", "", "", 0, 0, f.otherPersonal.Meta.ID, now); err == nil {
		t.Fatal("a key item ran in an account its principal does not hold")
	}
	p, err = st.auth.Recheck("", f.user, "", "jti", 1, now.Add(time.Hour).Unix(), f.personalAcct.Meta.ID, now)
	if err != nil || p.Caller.Lane.Meta.ID != f.personalAcct.Meta.ID {
		t.Fatalf("token item lane = %v, %v", p, err)
	}
	if _, err := st.auth.Recheck("", f.user, f.project.Meta.ID, "jti", 1, now.Add(time.Hour).Unix(), f.personalAcct.Meta.ID, now); err == nil {
		t.Fatal("a project token's item ran outside its project")
	}
}

// The /a/{account}/ prefix serves under every route Mount registers, profile routes and listings included, with no route of its own.
func TestMount_AccountPrefixReachesEveryRoute(t *testing.T) {
	r := mountWithRoutedProfile(t)
	for path, want := range map[string]int{
		"/a/acme-ml/healthz":         http.StatusOK,
		"/a/acme-ml/stub/api/hello":  http.StatusOK,
		"/a/acme-ml/v1/models":       http.StatusUnauthorized,
		"/a/acme-ml/stub/v1/models":  http.StatusUnauthorized,
		"/a/acme-ml/stub/api/hidden": http.StatusNotFound,
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}

// The prefix is cut from the escaped path too, so a route matching on it sees no prefix either.
func TestLaneMiddleware_CutsRawPath(t *testing.T) {
	var got *http.Request
	h := LaneMiddleware("")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r }))
	r := httptest.NewRequest(http.MethodPost, "/a/acme-ml/gemini/v1beta/models/a%2Fb:generateContent", nil)
	h.ServeHTTP(httptest.NewRecorder(), r)
	if got.URL.Path != "/gemini/v1beta/models/a/b:generateContent" || got.URL.RawPath != "/gemini/v1beta/models/a%2Fb:generateContent" {
		t.Fatalf("path %q raw %q", got.URL.Path, got.URL.RawPath)
	}
	if laneFrom(got.Context()) != "acme-ml" {
		t.Fatalf("lane = %q", laneFrom(got.Context()))
	}
	if r.URL.Path != "/a/acme-ml/gemini/v1beta/models/a/b:generateContent" {
		t.Fatalf("the caller's request was changed: %q", r.URL.Path)
	}
}

// The default path, with no switch, passes the request on untouched and allocates nothing.
func TestLaneMiddleware_NoSwitchAllocatesNothing(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Host = "relay.example.com"
	var got *http.Request
	h := LaneMiddleware("acc.example.com")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r }))
	if n := testing.AllocsPerRun(100, func() { h.ServeHTTP(nil, r) }); n != 0 {
		t.Fatalf("allocs = %v, want 0", n)
	}
	if got != r {
		t.Fatal("the request was copied")
	}
}

func BenchmarkLaneMiddleware_NoSwitch(b *testing.B) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Host = "relay.example.com"
	h := LaneMiddleware("acc.example.com")(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	b.ReportAllocs()
	for b.Loop() {
		h.ServeHTTP(nil, r)
	}
}
