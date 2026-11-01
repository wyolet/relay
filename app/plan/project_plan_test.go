package plan

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
)

// tokenCaller is the plan a token of u scoped to the basic fixture's project resolves to.
func (h *harness) tokenCaller(b *basic, userID string) *CallerPlan {
	h.t.Helper()
	root := h.holder.Load()
	pp, up := root.Projects.Lookup(b.proj.Meta.ID), root.Users.Lookup(userID)
	if pp == nil || up == nil {
		h.t.Fatalf("project plan %v, user plan %v", pp, up)
	}
	return pp.CallerFor(up.Caller)
}

// A token acts in its project: the project's and its team's policies narrow it, their counters count it, and their credentials are within its reach, above the user's own rung.
func TestProjectPlan_TokenCallerFoldsTheProjectAndTeam(t *testing.T) {
	b := newBasic()
	other := b.w.model(b.prov, "other")
	for _, h := range []*host.Host{b.hostA, b.hostB} {
		b.w.bind(other, h)
		b.w.price(other, h)
	}
	u := b.w.user()
	uk := b.w.userKey(u)
	projectCred := b.w.cred(b.hostB, b.acctB, meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID})
	bud := b.w.budget("50")
	rl := b.w.rateLimit(rule(ratelimit.MeterRequests, 10, time.Minute))
	teamPol := b.w.policy("team", policy.Spec{Models: []string{"acme/m1"}, BudgetIDs: []string{bud.Meta.ID}})
	b.w.bindPolicy(teamPol, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	userPol := b.w.policy("user", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	b.w.bindPolicy(userPol, subject(rolebinding.SubjectUser, u.ID))
	h := b.w.start(t)

	own := h.keyPlan(uk).Caller
	tok := h.tokenCaller(b, u.ID)
	if own.Team().SubjectID != "" || tok.Team().SubjectID != "team:"+b.team.Meta.ID || tok.Project().SubjectID != "project:"+b.proj.Meta.ID {
		t.Fatalf("rungs: own team %q; token team %q project %q", own.Team().SubjectID, tok.Team().SubjectID, tok.Project().SubjectID)
	}
	if h.tokenCaller(b, u.ID) != tok {
		t.Fatal("a second token request folded the plan again")
	}

	// The team's model list narrows the token, not the user's own key.
	wantHops(t, own.Routes.Get("other"), ids(b.a1), ids(b.a2), ids(b.b1))
	if r := tok.Routes.Get("other"); r.Refused == nil || r.Refused.Rung != limits.RungTeam {
		t.Fatalf("token route to a model the team denies = %+v, want refused on the team rung", r.Refused)
	}
	// The project's own credential is reached by ownership, innermost scope first.
	wantHops(t, tok.Routes.Get("m1"), ids(projectCred), ids(b.a1), ids(b.a2), ids(b.b1))

	// The team budget counts under the team; the user's own limit keeps the user's buckets, the same as from a personal key.
	var teamBudget, userRule *limits.Counter
	for i := range tok.Counters {
		switch c := &tok.Counters[i]; c.Rung {
		case limits.RungTeam:
			teamBudget = c
		case limits.RungPrincipal:
			userRule = c
		}
	}
	if teamBudget == nil || teamBudget.Kind != limits.KindBudget || teamBudget.Group != "team:"+b.team.Meta.ID {
		t.Fatalf("team budget counter = %+v, want one grouped under the team", teamBudget)
	}
	if userRule == nil || userRule.Group != "user:"+u.ID || userRule.ID != own.Counters[0].ID || userRule.Group != own.Counters[0].Group {
		t.Fatalf("user rule counter = %+v, want the user's own bucket %+v", userRule, own.Counters)
	}

	// A disabled team fails the token closed.
	off := false
	tm := clone(b.team)
	tm.Spec.Enabled = &off
	apply(h, tm)
	if r := h.tokenCaller(b, u.ID).Refused; r == nil || r.Code != CodeSubjectDisabled || r.Rung != limits.RungTeam {
		t.Fatalf("token under a disabled team: refused = %+v, want subject_disabled on the team rung", r)
	}
}

// A user's plans in projects live on the user's own plan: a catalog change rebuilds it, and a user who never calls again leaves no old token plan, and no catalog index it was folded over, kept by the project, which the change leaves as it was.
func TestProjectCallersGoWithTheUserPlan(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	h := b.w.start(t)
	project := h.holder.Load().Projects.Lookup(b.proj.Meta.ID)
	old := h.tokenCaller(b, u.ID)
	oldPlan, oldIndex := weak.Make(old), weak.Make(old.reads.idx)
	old = nil

	renamed := clone(b.m1)
	for range 3 {
		renamed = clone(renamed)
		renamed.Meta.DisplayName = "renamed-" + seq()
		apply(h, renamed)
	}
	if h.holder.Load().Projects.Lookup(b.proj.Meta.ID) != project {
		t.Fatal("a catalog change rebuilt the project plan; the test needs it kept")
	}
	if cached := h.holder.Load().Users.Lookup(u.ID).Caller.inProjects.m.Load(); cached != nil {
		t.Fatalf("the rebuilt user plan starts with %d project plans, want none", len(*cached))
	}
	runtime.GC()
	runtime.GC()
	if oldPlan.Value() != nil || oldIndex.Value() != nil {
		t.Fatalf("after the catalog changed: old token plan alive %v, old catalog index alive %v; want both released", oldPlan.Value() != nil, oldIndex.Value() != nil)
	}
	h.tokenCaller(b, u.ID)
	if n := len(*h.holder.Load().Users.Lookup(u.ID).Caller.inProjects.m.Load()); n != 1 {
		t.Fatalf("user plan caches %d project plans after one token request, want 1", n)
	}
}

// One user plan keeps at most maxProjectCallers project plans.
func TestProjectCallersAreBounded(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	projects := make([]string, maxProjectCallers+5)
	for i := range projects {
		projects[i] = b.w.project(b.team).Meta.ID
	}
	h := b.w.start(t)
	root := h.holder.Load()
	userPlan := root.Users.Lookup(u.ID).Caller
	for _, id := range projects {
		root.Projects.Lookup(id).CallerFor(userPlan)
	}
	if n := len(*userPlan.inProjects.m.Load()); n > maxProjectCallers {
		t.Fatalf("user plan caches %d project plans, want at most %d", n, maxProjectCallers)
	}
	last := root.Projects.Lookup(projects[len(projects)-1])
	if last.CallerFor(userPlan) != last.CallerFor(userPlan) {
		t.Fatal("the latest project's plan was folded again")
	}
}

// Token requests racing in several projects each get a plan, and once the cache settles every project answers with one plan.
func TestProjectCallersUnderConcurrency(t *testing.T) {
	b := newBasic()
	u := b.w.user()
	projects := []string{b.proj.Meta.ID, b.w.project(b.team).Meta.ID, b.w.project(b.team).Meta.ID}
	h := b.w.start(t)
	root := h.holder.Load()
	userPlan := root.Users.Lookup(u.ID).Caller
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for _, id := range projects {
				if root.Projects.Lookup(id).CallerFor(userPlan) == nil {
					t.Error("no token plan")
				}
			}
		})
	}
	wg.Wait()
	for _, id := range projects {
		p := root.Projects.Lookup(id)
		if p.CallerFor(userPlan) != p.CallerFor(userPlan) {
			t.Fatalf("project %s folded again after the cache settled", id)
		}
	}
}
