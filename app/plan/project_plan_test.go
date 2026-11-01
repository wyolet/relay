package plan

import (
	"testing"
	"time"

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
