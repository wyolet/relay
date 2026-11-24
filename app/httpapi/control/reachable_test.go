package control

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// reachAuthz allows every authenticated caller except writing a policy in noCreate, and hides rows owned by hidden.
type reachAuthz struct {
	authz.AlwaysAllowAuthenticated
	noCreate, hidden meta.Owner
}

func (a reachAuthz) Authorize(ctx context.Context, action string, r authz.Resource) error {
	if strings.HasPrefix(action, "policies.") && r.Owner != nil && *r.Owner == a.noCreate {
		return errors.New("forbidden")
	}
	return a.AlwaysAllowAuthenticated.Authorize(ctx, action, r)
}

func (a reachAuthz) Visible(_ context.Context, _, _ string, owner meta.Owner) bool {
	return owner != a.hidden
}

func reachRef(m meta.Metadata) ReachRef {
	return ReachRef{ID: m.ID, Name: m.Name, DisplayName: m.DisplayName}
}

// reachHarness serves the reach reads over w as caller u.
func reachHarness(t *testing.T, w *plantest.World, az authz.Authorizer) http.Handler {
	t.Helper()
	r, api := testAPI(t, map[string]*actor.Actor{"u": {UserID: meta.NewID(), Username: "u"}})
	registerReachable(api, Deps{Authz: az, PlanView: w.Load(t).Engine.View}, nil)
	return r
}

// wantReach reads path and compares the whole answer; an absent list and an empty one read the same.
func wantReach(t *testing.T, h http.Handler, path string, want Reachable) {
	t.Helper()
	var got Reachable
	wantJSON(t, scopeReq(t, h, "u", http.MethodGet, path, ""), http.StatusOK, &got)
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("GET %s (-want +got):\n%s", path, diff)
	}
}

type reachFixture struct {
	h            http.Handler
	team, other  *team.Team
	pa, pb, hide *pool.Pool
	saved        *policy.Policy
	teamAcct     ReachRef
	// m1 is served at host-a and host-b, m2 at host-a alone; viaA is all pool a brings.
	m1, m2, m1ViaA ReachedModel
}

// newReachFixture is host-a serving m1 and m2 and host-b serving m1, a pool per host, a pool of another team at host-b the caller cannot see, and a saved team policy linking pools a and b.
func newReachFixture(t *testing.T) *reachFixture {
	t.Helper()
	w := &plantest.World{}
	prov := w.Provider("acme")
	ha, hb := w.Host("host-a", "http://a.example"), w.Host("host-b", "http://b.example")
	m1, m2 := w.Model(prov, "m1"), w.Model(prov, "m2")
	w.Bind(m1, ha, adapters.OpenAI)
	w.Bind(m1, hb, adapters.OpenAI)
	w.Bind(m2, ha, adapters.OpenAI)
	f := &reachFixture{team: w.Team(), other: w.Team()}
	accA, accB := w.Account(ha), w.Account(hb)
	f.pa = w.Pool(ha, w.Credential(ha, accA, plantest.System, "sk-a"))
	f.pb = w.Pool(hb, w.Credential(hb, accB, plantest.System, "sk-b"))
	otherOwner := meta.Owner{Kind: meta.OwnerTeam, ID: f.other.Meta.ID}
	f.hide = w.Pool(hb, w.Credential(hb, w.Account(hb), plantest.System, "sk-hidden"))
	f.hide.Meta.Owner = otherOwner
	f.saved = w.Policy("team-default", policy.Spec{PoolIDs: []string{f.pa.Meta.ID, f.pb.Meta.ID}})
	f.saved.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}
	w.BindPolicy(f.saved, w.TeamAccount(f.team))
	f.teamAcct = reachRef(w.TeamAccount(f.team).Meta)

	a, b := reachRef(accA.Meta), reachRef(accB.Meta)
	viaA, viaB := ReachVia{Pool: ptr(reachRef(f.pa.Meta)), Account: &a}, ReachVia{Pool: ptr(reachRef(f.pb.Meta)), Account: &b}
	model := func(m meta.Metadata, hosts []ReachRef, via []ReachVia, upstream ...ReachRef) ReachedModel {
		out := ReachedModel{Provider: reachRef(prov.Meta), Model: ReachModelRef{ReachRef: reachRef(m)}}
		for _, h := range hosts {
			out.Hosts = append(out.Hosts, ReachHost{ReachRef: h, Snapshots: []string{m.Name}})
		}
		for _, v := range via {
			v.Snapshots = []string{m.Name}
			out.Via = append(out.Via, v)
		}
		for _, acc := range upstream {
			out.Upstream = append(out.Upstream, ReachUpstream{Account: acc, Allowed: true})
		}
		return out
	}
	f.m1 = model(m1.Meta, []ReachRef{reachRef(ha.Meta), reachRef(hb.Meta)}, []ReachVia{viaA, viaB}, a, b)
	f.m2 = model(m2.Meta, []ReachRef{reachRef(ha.Meta)}, []ReachVia{viaA}, a)
	f.m1ViaA = model(m1.Meta, []ReachRef{reachRef(ha.Meta)}, []ReachVia{viaA}, a)
	f.h = reachHarness(t, w, reachAuthz{noCreate: otherOwner, hidden: otherOwner})
	return f
}

func ptr[T any](v T) *T { return &v }

// boundTo is ms as a saved policy's read lists them: each with the bound accounts that reach it.
func boundTo(acct ReachRef, ms ...ReachedModel) []ReachedModel {
	for i := range ms {
		ms[i].Accounts = []ReachRef{acct}
	}
	return ms
}

// A draft, the saved policy with the same owner and pools, and the account bound to it answer the same models, hosts and upstream verdicts.
func TestReachable_DraftSavedAndAccountAgree(t *testing.T) {
	f := newReachFixture(t)
	models := []ReachedModel{f.m1, f.m2}
	wantReach(t, f.h, "/catalog/reachable?owner=team:"+f.team.Meta.ID+"&pool="+f.pa.Meta.ID+"&pool="+f.pb.Meta.ID, Reachable{Account: &f.teamAcct, Models: models})
	wantReach(t, f.h, "/accounts/"+f.teamAcct.ID+"/reachable", Reachable{Account: &f.teamAcct, Models: models})
	wantReach(t, f.h, "/accounts/"+f.teamAcct.Name+"/reachable", Reachable{Account: &f.teamAcct, Models: models})
	for _, ref := range []string{f.saved.Meta.ID, f.saved.Meta.Name} {
		wantReach(t, f.h, "/policies/"+ref+"/reachable", Reachable{BoundAccounts: []ReachRef{f.teamAcct}, Models: boundTo(f.teamAcct, f.m1, f.m2)})
	}
}

func TestReachable_ListsAreNeverNull(t *testing.T) {
	f := newReachFixture(t)
	for _, path := range []string{"/catalog/reachable?owner=team:" + f.team.Meta.ID + "&pool=" + f.pa.Meta.ID, "/policies/" + f.saved.Meta.ID + "/reachable", "/accounts/" + f.teamAcct.ID + "/reachable"} {
		if body := scopeReq(t, f.h, "u", http.MethodGet, path, "").Body.String(); strings.Contains(body, "null") {
			t.Errorf("GET %s: %s", path, body)
		}
	}
}

func TestReachable_Refusals(t *testing.T) {
	f := newReachFixture(t)
	get := func(path string) *httptest.ResponseRecorder { return scopeReq(t, f.h, "u", http.MethodGet, path, "") }
	wantAPIError(t, get("/catalog/reachable?owner=team:"+f.other.Meta.ID), http.StatusForbidden, "", "you may not write a policy owned by team:"+f.other.Meta.ID)
	wantAPIError(t, get("/catalog/reachable?owner=team:"+f.team.Meta.ID+"&account="+f.teamAcct.Name), http.StatusBadRequest, "", `account "`+f.teamAcct.Name+`" is no account inside team:`+f.team.Meta.ID+` that you may read`)
	wantAPIError(t, get("/accounts/nope/reachable"), http.StatusNotFound, "", `account "nope" not found`)
	wantAPIError(t, get("/policies/nope/reachable"), http.StatusNotFound, "", `policy "nope" not found`)
	if w := get("/catalog/reachable?owner=host:x"); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an owner kind policies cannot have = %d, want 422: %s", w.Code, w.Body)
	}
}

// Each reached host and pool names the snapshots whose requests it serves, so a host serving one snapshot of a model is told apart from one serving another.
func TestReachable_HostsNameTheSnapshotsTheyServe(t *testing.T) {
	w := &plantest.World{}
	prov := w.Provider("acme")
	ha, hb := w.Host("host-a", "http://a.example"), w.Host("host-b", "http://b.example")
	m := w.Model(prov, "m1", "m1-remote")
	w.Bind(m, ha, adapters.OpenAI, "m1-remote")
	w.Bind(m, hb, adapters.OpenAI, "m1")
	tm := w.Team()
	accA, accB := w.Account(ha), w.Account(hb)
	pa := w.Pool(ha, w.Credential(ha, accA, plantest.System, "sk-a"))
	pb := w.Pool(hb, w.Credential(hb, accB, plantest.System, "sk-b"))
	pol := w.Policy("team-default", policy.Spec{PoolIDs: []string{pa.Meta.ID, pb.Meta.ID}})
	pol.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}
	w.BindPolicy(pol, w.TeamAccount(tm))
	teamAcct := reachRef(w.TeamAccount(tm).Meta)
	h := reachHarness(t, w, authz.AlwaysAllowAuthenticated{})

	a, b := reachRef(accA.Meta), reachRef(accB.Meta)
	wantReach(t, h, "/accounts/"+teamAcct.ID+"/reachable", Reachable{Account: &teamAcct, Models: []ReachedModel{{
		Provider: reachRef(prov.Meta), Model: ReachModelRef{ReachRef: reachRef(m.Meta)},
		Hosts: []ReachHost{{ReachRef: reachRef(ha.Meta), Snapshots: []string{"m1-remote"}}, {ReachRef: reachRef(hb.Meta), Snapshots: []string{"m1"}}},
		Via: []ReachVia{
			{Pool: ptr(reachRef(pa.Meta)), Account: &a, Snapshots: []string{"m1-remote"}},
			{Pool: ptr(reachRef(pb.Meta)), Account: &b, Snapshots: []string{"m1"}},
		},
		Upstream: []ReachUpstream{{Account: a, Allowed: true}, {Account: b, Allowed: true}},
	}}})
}

// A pool the caller cannot read is left out of the draft with everything only it brings, and is dropped by id alone.
func TestReachable_OmitsUnreadablePools(t *testing.T) {
	f := newReachFixture(t)
	wantReach(t, f.h, "/catalog/reachable?owner=team:"+f.team.Meta.ID+"&pool="+f.pa.Meta.ID+"&pool="+f.hide.Meta.ID+"&pool=gone", Reachable{
		Account: &f.teamAcct, Models: []ReachedModel{f.m1ViaA, f.m2},
		Dropped: []ReachDropped{
			{Pool: &ReachRef{ID: f.hide.Meta.ID}, Code: "not_found", Reason: "no pool " + f.hide.Meta.ID + " that you may read"},
			{Pool: &ReachRef{ID: "gone"}, Code: "not_found", Reason: "no pool gone that you may read"},
		},
	})
}

// A project policy bound to a service account, whose pool lists an account shared to that service account: the share serves only its lane.
func TestReachable_ShareHeldForAServiceAccount(t *testing.T) {
	w := &plantest.World{}
	prov, h := w.Provider("acme"), w.Host("openai", "http://openai.example")
	m := w.Model(prov, "gpt-4o-mini")
	w.Bind(m, h, adapters.OpenAI)
	real := w.Account(h)
	w.Credential(h, real, plantest.System, "sk-real")
	p := w.Project(w.Team())
	sa := w.ServiceAccount(p)
	projOwner := meta.Owner{Kind: meta.OwnerProject, ID: p.Meta.ID}
	share := account.Share(real, account.Receiver{Kind: account.ReceiverServiceAccount, ID: sa.Meta.ID}, projOwner, sa.Meta.Name)
	share.Meta.ID = meta.NewID()
	w.Accounts = append(w.Accounts, share)
	pl := w.Pool(h)
	pl.Meta.Owner, pl.Spec.AccountIDs = projOwner, []string{share.Meta.ID}
	pol := w.Policy("web-ci", policy.Spec{PoolIDs: []string{pl.Meta.ID}})
	pol.Meta.Owner = projOwner
	w.BindPolicy(pol, w.ServiceAccountAccount(sa))
	otherTeam := w.TeamAccount(w.Team()).Meta.ID
	h2 := reachHarness(t, w, authz.AlwaysAllowAuthenticated{})

	projAcct, saAcct, shared := reachRef(w.ProjectAccount(p).Meta), reachRef(w.ServiceAccountAccount(sa).Meta), reachRef(share.Meta)
	reached := ReachedModel{
		Provider: reachRef(prov.Meta), Model: ReachModelRef{ReachRef: reachRef(m.Meta)}, Hosts: []ReachHost{{ReachRef: reachRef(h.Meta), Snapshots: []string{m.Meta.Name}}},
		Via:      []ReachVia{{Pool: ptr(reachRef(pl.Meta)), Account: &shared, Snapshots: []string{m.Meta.Name}}},
		Upstream: []ReachUpstream{{Account: reachRef(real.Meta), SharedAs: &shared, Allowed: true}},
	}
	draft := "/catalog/reachable?owner=project:" + p.Meta.ID + "&pool=" + pl.Meta.ID
	// Read in the project's account the share serves nothing, and dropped says why and what to pass.
	wantReach(t, h2, draft, Reachable{Account: &projAcct, Dropped: []ReachDropped{{
		Pool: ptr(reachRef(pl.Meta)), Account: &shared, Code: "held_for_another_lane",
		Reason: "shared account " + share.Meta.Name + " is held for service account " + sa.Meta.Name + "; it serves only that account's lane: pass account=" + saAcct.ID,
	}}})
	wantReach(t, h2, draft+"&account="+saAcct.ID, Reachable{Account: &saAcct, Models: []ReachedModel{reached}})
	wantReach(t, h2, "/policies/"+pol.Meta.ID+"/reachable", Reachable{BoundAccounts: []ReachRef{saAcct}, Models: boundTo(saAcct, reached)})
	wantAPIError(t, scopeReq(t, h2, "u", http.MethodGet, draft+"&account="+otherTeam, ""), http.StatusBadRequest, "",
		`account "`+otherTeam+`" is no account inside project:`+p.Meta.ID+` that you may read`)
}
