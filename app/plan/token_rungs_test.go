package plan_test

import (
	"testing"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
)

// A token runs in the user's member account in its project, under the project's and the team's accounts: all three narrow, while the user's personal key carries only the personal account.
func TestToken_LaneClimbsMemberProjectAndTeam(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	u := o.w.User()
	member := o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
	k := o.w.UserKey(u, "sk-alice")
	teamPol := o.w.Policy("team", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	projPol := o.w.Policy("project", policy.Spec{Models: []string{"prov/m2", "prov/m3"}})
	memberPol := o.w.Policy("member", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	personalPol := o.w.Policy("personal", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	o.w.BindPolicy(teamPol, o.teamAcct())
	o.w.BindPolicy(projPol, o.projectAcct())
	o.w.BindPolicy(memberPol, member)
	o.w.BindPolicy(personalPol, o.w.PersonalAccount(u))
	plans := o.w.Load(t)

	tok := tokenCaller(t, plans, u.ID, o.proj.Meta.ID)
	if tok.Team().SubjectID != "account:"+o.teamAcct().Meta.ID || tok.Project().SubjectID != "account:"+o.projectAcct().Meta.ID || tok.LaneRung().SubjectID != "account:"+member.Meta.ID {
		t.Fatalf("token rungs = %q / %q / %q, want the team's, the project's and the member account", tok.Team().SubjectID, tok.Project().SubjectID, tok.LaneRung().SubjectID)
	}
	if tok.Team().ID() != o.team.Meta.ID || tok.Project().ID() != o.proj.Meta.ID {
		t.Fatalf("token scope = team %q project %q", tok.Team().ID(), tok.Project().ID())
	}
	mustRoute(t, tok, "m2")
	wantRefusal(t, tok.Routes.Get("m1"), plan.CodeModelNotAllowed, limits.RungProject, projPol.Meta.ID)
	if r := tok.Routes.Get("m3"); r.Refused == nil {
		t.Fatal("token reached m3, which the team and member accounts refuse")
	}

	personal := keyCaller(t, plans, k)
	mustRoute(t, personal, "m1")
	mustRoute(t, personal, "m2")
	wantRefusal(t, personal.Routes.Get("m3"), plan.CodeModelNotAllowed, limits.RungLane, personalPol.Meta.ID)
}

// Without a caller policy a member lane reaches what its project and team own, plus the system's, innermost owner first; the user's own keys are the personal account's, not the member's.
func TestToken_ReachesProjectTeamAndSystemCredentials(t *testing.T) {
	o := newOrg()
	u := o.w.User()
	o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
	sys := o.w.Credential(o.host, nil, plantest.System, "sk-system")
	tm := o.w.Credential(o.host, nil, ownedBy(meta.OwnerTeam, o.team.Meta.ID), "sk-team")
	pr := o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerUser, u.ID), "sk-own")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.w.Project(o.team).Meta.ID), "sk-sibling")
	plans := o.w.Load(t)

	want := []string{pr.Meta.Name, tm.Meta.Name, sys.Meta.Name}
	if got := routeCredentials(mustRoute(t, tokenCaller(t, plans, u.ID, o.proj.Meta.ID), "m1")); !sameList(got, want) {
		t.Fatalf("token reaches %v, want %v", got, want)
	}
}

// A user with no account in the project has no lane there: the token is refused, never routed through another account.
func TestToken_NoMemberAccountIsNotHeld(t *testing.T) {
	o := newOrg()
	u := o.w.User()
	plans := o.w.Load(t)
	if _, refused := plans.Root().MemberLane(u.ID, o.proj.Meta.ID); refused == nil || refused.Code != plan.CodeAccountNotHeld {
		t.Fatalf("lane of a non-member = %v, want account_not_held", refused)
	}
}

// A disabled team refuses every lane under it.
func TestToken_DisabledTeamRefuses(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	o.team.Spec.Enabled = off()
	u := o.w.User()
	o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
	plans := o.w.Load(t)

	r := tokenCaller(t, plans, u.ID, o.proj.Meta.ID).Routes.Get("m1")
	if r.Refused == nil || r.Refused.Code != plan.CodeSubjectDisabled || r.Refused.Rung != limits.RungTeam {
		t.Fatalf("token under a disabled team: refusal %+v, want subject_disabled on the team rung", r.Refused)
	}
}

// A disabled project refuses every lane in it.
func TestToken_DisabledProjectRefuses(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	o.proj.Spec.Enabled = off()
	u := o.w.User()
	o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
	plans := o.w.Load(t)

	if r := tokenCaller(t, plans, u.ID, o.proj.Meta.ID).Routes.Get("m1"); r.Refused == nil || r.Refused.Rung != limits.RungProject {
		t.Fatalf("a token in a disabled project = %+v, want a refusal on the project rung", r.Refused)
	}
}

// A disabled team refuses its service accounts' keys too, naming the team rung.
func TestKey_DisabledTeamRefusesItsServiceAccounts(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	o.team.Spec.Enabled = off()
	cp := keyCaller(t, o.w.Load(t), o.saKey)
	r := cp.Routes.Get("m1")
	if r.Refused == nil || r.Refused.Rung != limits.RungTeam {
		t.Fatalf("service account under a disabled team: refusal %+v, want one on the team rung", r.Refused)
	}
}
