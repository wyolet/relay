package plan_test

import (
	"slices"
	"testing"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
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
	wantRefusal(t, tok.Routes.Get("m3"), plan.CodeModelNotAllowed, limits.RungTeam, teamPol.Meta.ID)

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
	if got := routeCredentials(mustRoute(t, tokenCaller(t, plans, u.ID, o.proj.Meta.ID), "m1")); !slices.Equal(got, want) {
		t.Fatalf("token reaches %v, want %v", got, want)
	}
}

// A user with no account in the project has no lane there: the token is refused, never routed through another account.
func TestToken_NoMemberAccountIsNotHeld(t *testing.T) {
	o := newOrg()
	u := o.w.User()
	cp, refused := o.w.Load(t).Root().MemberLane(u.ID, o.proj.Meta.ID)
	want := plan.Refusal{Code: plan.CodeAccountNotHeld, Message: "user:" + u.ID + " holds no account in project " + o.proj.Meta.ID}
	if cp != nil || refused == nil || *refused != want {
		t.Fatalf("lane of a non-member = %v, %+v; want none and %+v", cp, refused, want)
	}
}

// A disabled team or project refuses every lane under it, a member's token and a service account's key alike, naming its rung.
func TestLadder_DisabledScopeRefusesEveryLaneUnderIt(t *testing.T) {
	for _, rung := range []limits.Rung{limits.RungTeam, limits.RungProject} {
		t.Run(string(rung), func(t *testing.T) {
			o := newOrg()
			o.w.Credential(o.host, nil, plantest.System, "sk-system")
			subject := "account:" + o.teamAcct().Meta.ID
			if rung == limits.RungTeam {
				o.team.Spec.Enabled = off()
			} else {
				o.proj.Spec.Enabled, subject = off(), "account:"+o.projectAcct().Meta.ID
			}
			u := o.w.User()
			o.w.Member(u, ownedBy(meta.OwnerProject, o.proj.Meta.ID))
			plans := o.w.Load(t)
			for name, cp := range map[string]*plan.CallerPlan{"token": tokenCaller(t, plans, u.ID, o.proj.Meta.ID), "service account key": keyCaller(t, plans, o.saKey)} {
				r := cp.Routes.Get("m1").Refused
				if r == nil || r.Code != plan.CodeSubjectDisabled || r.Rung != rung || r.SubjectID != subject {
					t.Errorf("%s: refusal %+v, want subject_disabled on the %s rung of %s", name, r, rung, subject)
				}
			}
		})
	}
}
