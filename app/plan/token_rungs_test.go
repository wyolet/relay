package plan_test

import (
	"testing"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/rolebinding"
)

// A token's plan is the user's rung under the project's and the team's: all three narrow, while the user's personal key carries only the user's own rung.
func TestToken_PlanFoldsUserProjectAndTeamRungs(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	u := o.w.User()
	k := o.w.UserKey(u, "sk-alice")
	teamPol := o.w.Policy("team", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	projPol := o.w.Policy("project", policy.Spec{Models: []string{"prov/m2", "prov/m3"}})
	userPol := o.w.Policy("user", policy.Spec{Models: []string{"prov/m1", "prov/m2"}})
	o.w.BindPolicy(teamPol, teamSubject(o.team))
	o.w.BindPolicy(projPol, projectSubject(o.proj))
	o.w.BindPolicy(userPol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	plans := o.w.Load(t)

	tok := tokenCaller(t, plans, u.ID, o.proj.Meta.ID)
	if tok == nil {
		t.Fatal("no token plan for an enabled project")
	}
	if tok.Team().SubjectID != "team:"+o.team.Meta.ID || tok.Project().SubjectID != "project:"+o.proj.Meta.ID || tok.PrincipalRung().SubjectID != "user:"+u.ID {
		t.Fatalf("token rungs = %q / %q / %q, want the team, the project and the user", tok.Team().SubjectID, tok.Project().SubjectID, tok.PrincipalRung().SubjectID)
	}
	mustRoute(t, tok, "m2")
	wantRefusal(t, tok.Routes.Get("m1"), plan.CodeModelNotAllowed, limits.RungProject, projPol.Meta.ID)
	if r := tok.Routes.Get("m3"); r.Refused == nil {
		t.Fatal("token reached m3, which the team and user rungs refuse")
	}

	personal := keyCaller(t, plans, k)
	mustRoute(t, personal, "m1")
	mustRoute(t, personal, "m2")
	wantRefusal(t, personal.Routes.Get("m3"), plan.CodeModelNotAllowed, limits.RungPrincipal, userPol.Meta.ID)
}

// Without a caller policy a token reaches what the user, the project and the team own, plus the system's, innermost owner first.
func TestToken_ReachesUserProjectTeamAndSystemCredentials(t *testing.T) {
	o := newOrg()
	u := o.w.User()
	sys := o.w.Credential(o.host, nil, plantest.System, "sk-system")
	tm := o.w.Credential(o.host, nil, ownedBy(meta.OwnerTeam, o.team.Meta.ID), "sk-team")
	pr := o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.proj.Meta.ID), "sk-project")
	own := o.w.Credential(o.host, nil, ownedBy(meta.OwnerUser, u.ID), "sk-own")
	o.w.Credential(o.host, nil, ownedBy(meta.OwnerProject, o.w.Project(o.team).Meta.ID), "sk-sibling")
	plans := o.w.Load(t)

	want := []string{own.Meta.Name, pr.Meta.Name, tm.Meta.Name, sys.Meta.Name}
	if got := routeCredentials(mustRoute(t, tokenCaller(t, plans, u.ID, o.proj.Meta.ID), "m1")); !sameList(got, want) {
		t.Fatalf("token reaches %v, want %v", got, want)
	}
}

// A disabled team refuses every token in its projects.
func TestToken_DisabledTeamRefuses(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	o.team.Spec.Enabled = off()
	u := o.w.User()
	plans := o.w.Load(t)

	cp := tokenCaller(t, plans, u.ID, o.proj.Meta.ID)
	if cp == nil {
		return // the project takes no tokens at all
	}
	r := cp.Routes.Get("m1")
	if r.Refused == nil || r.Refused.Code != plan.CodeSubjectDisabled || r.Refused.Rung != limits.RungTeam {
		t.Fatalf("token under a disabled team: refusal %+v, want subject_disabled on the team rung", r.Refused)
	}
}

// A disabled project refuses every token in it.
func TestToken_DisabledProjectRefuses(t *testing.T) {
	o := newOrg()
	o.w.Credential(o.host, nil, plantest.System, "sk-system")
	o.proj.Spec.Enabled = off()
	u := o.w.User()
	plans := o.w.Load(t)

	cp := tokenCaller(t, plans, u.ID, o.proj.Meta.ID)
	if cp == nil {
		return
	}
	if r := cp.Routes.Get("m1"); r.Refused == nil {
		t.Fatal("a token in a disabled project was served")
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
