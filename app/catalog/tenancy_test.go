package catalog

import (
	"context"
	"reflect"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/team"
)

type teamList []*team.Team

func (l teamList) List(context.Context) ([]*team.Team, error) { return l, nil }

type projList []*project.Project

func (l projList) List(context.Context) ([]*project.Project, error) { return l, nil }

// tenancyFixture is one team with two projects (named so that insertion
// order and sorted order differ) plus one project-owned row of every kind
// that can live in a project.
type tenancyFixture struct {
	team     *team.Team
	other    *team.Team
	zeta     *project.Project
	alpha    *project.Project
	orphan   *project.Project
	pol      *policy.Policy
	hostTier *policy.Policy
	key      *hostkey.HostKey
	rl       *ratelimit.RateLimit
}

func newTenancyFixture() tenancyFixture {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	other := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "research", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	newProject := func(name, teamID string) *project.Project {
		p := &project.Project{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: project.Spec{TeamID: teamID},
		}
		p.StampOwner()
		return p
	}
	zeta := newProject("zeta", tm.Meta.ID)
	alpha := newProject("alpha", tm.Meta.ID)
	orphan := newProject("orphan", meta.NewID())
	owner := meta.Owner{Kind: meta.OwnerProject, ID: zeta.Meta.ID}
	return tenancyFixture{
		team: tm, other: other, zeta: zeta, alpha: alpha, orphan: orphan,
		pol: &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "zeta-pol", Owner: owner}},
		rl: &ratelimit.RateLimit{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "zeta-rl", Owner: owner},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
				Meter: ratelimit.MeterRequests, Amount: 10, Window: 60, Strategy: ratelimit.StrategyTokenBucket,
			}}},
		},
	}
}

func (f tenancyFixture) catalog(t *testing.T) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{f.rl}, rkList{}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team, f.other}, projList{f.zeta, f.alpha, f.orphan})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

func projectNames(ps []*project.Project) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Meta.Name)
	}
	return out
}

// Rule 3: a project whose team is absent (or disabled) is dropped, and
// ProjectsInTeam is ordered by project name.
func TestBuild_ProjectDroppedWithoutTeam(t *testing.T) {
	f := newTenancyFixture()
	s := f.catalog(t).Current()

	if _, ok := s.Project(f.orphan.Meta.ID); ok {
		t.Error("project with an unknown team is in the snapshot")
	}
	if got := projectNames(s.ProjectsInTeam(f.team.Meta.ID)); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Errorf("ProjectsInTeam = %v, want [alpha zeta]", got)
	}

	disabled := false
	f2 := newTenancyFixture()
	f2.team.Spec.Enabled = &disabled
	s2 := f2.catalog(t).Current()
	if _, ok := s2.Project(f2.zeta.Meta.ID); ok {
		t.Error("project of a disabled team is in the snapshot")
	}
	if _, ok := s2.Team(f2.team.Meta.ID); ok {
		t.Error("disabled team is in the snapshot")
	}
}

// Rule 4: the scope chain of every owner kind.
func TestScopeChain(t *testing.T) {
	f := newTenancyFixture()
	s := f.catalog(t).Current()

	for _, tc := range []struct {
		name  string
		owner *meta.Owner
		want  []meta.Owner
	}{
		{
			name:  "project",
			owner: &meta.Owner{Kind: meta.OwnerProject, ID: f.zeta.Meta.ID},
			want: []meta.Owner{
				{Kind: meta.OwnerProject, ID: f.zeta.Meta.ID},
				{Kind: meta.OwnerTeam, ID: f.team.Meta.ID},
				global,
			},
		},
		{
			name:  "team",
			owner: &meta.Owner{Kind: meta.OwnerTeam, ID: f.team.Meta.ID},
			want:  []meta.Owner{{Kind: meta.OwnerTeam, ID: f.team.Meta.ID}, global},
		},
		{name: "user", owner: &meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}, want: []meta.Owner{global}},
		{name: "system", owner: &meta.Owner{Kind: meta.OwnerSystem}, want: []meta.Owner{global}},
		{name: "host", owner: &meta.Owner{Kind: meta.OwnerHost, ID: "h-1"}, want: []meta.Owner{global}},
		{name: "nil", owner: nil, want: []meta.Owner{global}},
		{
			name:  "absent project",
			owner: &meta.Owner{Kind: meta.OwnerProject, ID: f.orphan.Meta.ID},
			want:  []meta.Owner{global},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.ScopeChain(tc.owner); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ScopeChain = %v, want %v", got, tc.want)
			}
		})
	}
}

// Rule 5: deleting (or disabling) a team evicts its projects and every
// project-owned row; moving a project re-indexes projectsByTeam.
func TestApplyTeamDelete_EvictsSubtree(t *testing.T) {
	f := newTenancyFixture()
	c := f.catalog(t)

	if err := c.ApplyTeamDelete(f.team.Meta.ID); err != nil {
		t.Fatalf("ApplyTeamDelete: %v", err)
	}
	s := c.Current()
	if _, ok := s.Team(f.team.Meta.ID); ok {
		t.Error("team still present")
	}
	for _, p := range []*project.Project{f.zeta, f.alpha} {
		if _, ok := s.Project(p.Meta.ID); ok {
			t.Errorf("project %q survived its team", p.Meta.Name)
		}
	}
	if _, ok := s.Policy(f.pol.Meta.ID); ok {
		t.Error("project-owned policy survived its team")
	}
	if _, ok := s.RateLimit(f.rl.Meta.ID); ok {
		t.Error("project-owned rate limit survived its team")
	}
}

// Invariant: the reconcile fixpoint after a delete equals a fresh build of
// the same inputs.
func TestApplyTeamDelete_MatchesBuild(t *testing.T) {
	f := newTenancyFixture()
	c := f.catalog(t)
	if err := c.ApplyTeamDelete(f.team.Meta.ID); err != nil {
		t.Fatalf("ApplyTeamDelete: %v", err)
	}
	reconciled := c.Current()

	fresh := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{f.rl}, rkList{}, rcList{}, bndList{})
	fresh.UseTenancy(teamList{f.other}, projList{f.zeta, f.alpha, f.orphan})
	if err := fresh.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	built := fresh.Current()

	if len(reconciled.AllProjects()) != len(built.AllProjects()) {
		t.Errorf("projects: reconcile %v, build %v", projectNames(reconciled.AllProjects()), projectNames(built.AllProjects()))
	}
	if len(reconciled.AllPolicies()) != len(built.AllPolicies()) {
		t.Errorf("policies: reconcile %d, build %d", len(reconciled.AllPolicies()), len(built.AllPolicies()))
	}
	if len(reconciled.AllRateLimits()) != len(built.AllRateLimits()) {
		t.Errorf("rate limits: reconcile %d, build %d", len(reconciled.AllRateLimits()), len(built.AllRateLimits()))
	}
}

func TestApplyTeamUpsert_DisabledIsDelete(t *testing.T) {
	f := newTenancyFixture()
	c := f.catalog(t)

	disabled := false
	off := *f.team
	off.Spec.Enabled = &disabled
	if err := c.ApplyTeamUpsert(&off); err != nil {
		t.Fatalf("ApplyTeamUpsert: %v", err)
	}
	s := c.Current()
	if _, ok := s.Team(f.team.Meta.ID); ok {
		t.Error("disabled team still present")
	}
	if _, ok := s.Project(f.zeta.Meta.ID); ok {
		t.Error("project of a disabled team still present")
	}
}

func TestApplyProjectUpsert_MovesTeam(t *testing.T) {
	f := newTenancyFixture()
	c := f.catalog(t)

	moved := *f.zeta
	moved.Spec.TeamID = f.other.Meta.ID
	moved.StampOwner()
	if err := c.ApplyProjectUpsert(&moved); err != nil {
		t.Fatalf("ApplyProjectUpsert: %v", err)
	}
	s := c.Current()
	if got := projectNames(s.ProjectsInTeam(f.team.Meta.ID)); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("old team projects = %v, want [alpha]", got)
	}
	if got := projectNames(s.ProjectsInTeam(f.other.Meta.ID)); !reflect.DeepEqual(got, []string{"zeta"}) {
		t.Errorf("new team projects = %v, want [zeta]", got)
	}
}

// Rule 6: a NOTIFY payload reaches the matching Apply method.
func TestApplyEvent_Tenancy(t *testing.T) {
	f := newTenancyFixture()
	c := f.catalog(t)
	l := NewListener(c, nil, listenerStores{
		team:    teamGetter{f.team},
		project: projectGetter{},
	})

	renamed := *f.team
	renamed.Meta.DisplayName = "Platform Engineering"
	l.stores.team = teamGetter{&renamed}
	if err := l.applyEvent(context.Background(), drainedEvent{Kind: "team", ID: f.team.Meta.ID, Op: "upsert"}); err != nil {
		t.Fatalf("applyEvent team upsert: %v", err)
	}
	if got, _ := c.Current().Team(f.team.Meta.ID); got.Meta.DisplayName != "Platform Engineering" {
		t.Errorf("team upsert did not reach ApplyTeamUpsert")
	}

	if err := l.applyEvent(context.Background(), drainedEvent{Kind: "project", ID: f.zeta.Meta.ID, Op: "delete"}); err != nil {
		t.Fatalf("applyEvent project delete: %v", err)
	}
	if _, ok := c.Current().Project(f.zeta.Meta.ID); ok {
		t.Error("project delete did not reach ApplyProjectDelete")
	}
}

type teamGetter struct{ t *team.Team }

func (g teamGetter) Get(context.Context, string) (*team.Team, error) { return g.t, nil }

type projectGetter struct{ p *project.Project }

func (g projectGetter) Get(context.Context, string) (*project.Project, error) { return g.p, nil }
