package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
)

type roleList []*role.Role

func (l roleList) List(context.Context) ([]*role.Role, error) { return l, nil }

type rbList []*rolebinding.RoleBinding

func (l rbList) List(context.Context) ([]*rolebinding.RoleBinding, error) { return l, nil }

type pbList []*policybinding.PolicyBinding

func (l pbList) List(context.Context) ([]*policybinding.PolicyBinding, error) { return l, nil }

// rbacFixture is one team with a live and a disabled project, one role, and
// the bindings that exercise membership, subject indexing, and priority.
type rbacFixture struct {
	team     *team.Team
	live     *project.Project
	disabled *project.Project
	pol      *policy.Policy
	role     *role.Role

	global    *rolebinding.RoleBinding
	teamWide  *rolebinding.RoleBinding
	inProject *rolebinding.RoleBinding
	noRole    *rolebinding.RoleBinding
	onDead    *rolebinding.RoleBinding

	low     *policybinding.PolicyBinding
	highA   *policybinding.PolicyBinding
	highB   *policybinding.PolicyBinding
	noPol   *policybinding.PolicyBinding
	onDeadP *policybinding.PolicyBinding
}

func newRBACFixture() rbacFixture {
	disabled := false
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	live := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	live.StampOwner()
	dead := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "retired"},
		Spec: project.Spec{TeamID: tm.Meta.ID, Enabled: &disabled},
	}
	dead.StampOwner()
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search-default", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	r := &role.Role{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "viewer", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}},
	}

	group := func(name string) rolebinding.Subject {
		return rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: name}
	}
	binding := func(name, roleID string, scope meta.Owner, subs ...rolebinding.Subject) *rolebinding.RoleBinding {
		b := &rolebinding.RoleBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: rolebinding.Spec{RoleID: roleID, Scope: scope, Subjects: subs},
		}
		b.StampOwner()
		return b
	}
	pbinding := func(name, projectID, policyID string, priority int) *policybinding.PolicyBinding {
		b := &policybinding.PolicyBinding{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name},
			Spec: policybinding.Spec{
				ProjectID: projectID, PolicyID: policyID, Priority: priority,
				Subjects: []rolebinding.Subject{group("system:authenticated")},
			},
		}
		b.StampOwner()
		return b
	}

	eng := group("platform-eng")
	return rbacFixture{
		team: tm, live: live, disabled: dead, pol: pol, role: r,

		global:    binding("zeta-global", r.Meta.ID, meta.Owner{Kind: meta.OwnerSystem}, eng),
		teamWide:  binding("alpha-team", r.Meta.ID, meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}, eng),
		inProject: binding("mid-project", r.Meta.ID, meta.Owner{Kind: meta.OwnerProject, ID: live.Meta.ID}, eng),
		noRole:    binding("dangling-role", meta.NewID(), meta.Owner{Kind: meta.OwnerSystem}, eng),
		onDead:    binding("on-disabled", r.Meta.ID, meta.Owner{Kind: meta.OwnerProject, ID: dead.Meta.ID}, eng),

		low:     pbinding("zeta-low", live.Meta.ID, pol.Meta.ID, 10),
		highA:   pbinding("alpha-high", live.Meta.ID, pol.Meta.ID, 500),
		highB:   pbinding("beta-high", live.Meta.ID, pol.Meta.ID, 500),
		noPol:   pbinding("dangling-policy", live.Meta.ID, meta.NewID(), 0),
		onDeadP: pbinding("on-disabled", dead.Meta.ID, pol.Meta.ID, 0),
	}
}

func (f rbacFixture) catalog(t *testing.T) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{}, polList{f.pol}, modList{}, keyList{}, rlList{}, rkList{}, rcList{}, bndList{})
	c.UseTenancy(teamList{f.team}, projList{f.live, f.disabled}, saList{}, grpList{},
		roleList{f.role},
		rbList{f.global, f.teamWide, f.inProject, f.noRole, f.onDead},
		pbList{f.low, f.highA, f.highB, f.noPol, f.onDeadP})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

func bindingNames(bs []*rolebinding.RoleBinding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Meta.Name)
	}
	return out
}

func policyBindingNames(bs []*policybinding.PolicyBinding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Meta.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuild_BindingMembership(t *testing.T) {
	f := newRBACFixture()
	s := f.catalog(t).Current()

	if _, ok := s.RoleBinding(f.noRole.Meta.ID); ok {
		t.Error("binding whose role is absent should be dropped")
	}
	if _, ok := s.RoleBinding(f.onDead.Meta.ID); ok {
		t.Error("binding scoped to a disabled project should be dropped")
	}
	if _, ok := s.PolicyBinding(f.noPol.Meta.ID); ok {
		t.Error("policy binding whose policy is absent should be dropped")
	}
	if _, ok := s.PolicyBinding(f.onDeadP.Meta.ID); ok {
		t.Error("policy binding in a disabled project should be dropped")
	}
	if _, ok := s.RoleByName("viewer"); !ok {
		t.Error("role should be indexed by name")
	}
}

func TestSnapshot_RoleBindingsForSubject(t *testing.T) {
	f := newRBACFixture()
	s := f.catalog(t).Current()

	got := bindingNames(s.RoleBindingsForSubject("group:platform-eng"))
	want := []string{"alpha-team", "mid-project", "zeta-global"}
	if !equalStrings(got, want) {
		t.Errorf("RoleBindingsForSubject = %v, want %v (sorted by name, across scopes)", got, want)
	}
	if got := s.RoleBindingsForSubject("group:nobody"); len(got) != 0 {
		t.Errorf("unknown subject = %v, want none", bindingNames(got))
	}
}

func TestSnapshot_PolicyBindingsForProject(t *testing.T) {
	f := newRBACFixture()
	s := f.catalog(t).Current()

	got := policyBindingNames(s.PolicyBindingsForProject(f.live.Meta.ID))
	want := []string{"zeta-low", "alpha-high", "beta-high"}
	if !equalStrings(got, want) {
		t.Errorf("PolicyBindingsForProject = %v, want %v (priority, then name)", got, want)
	}
}

func TestReconcile_RoleDeleteEvictsBindings(t *testing.T) {
	f := newRBACFixture()
	c := f.catalog(t)

	if err := c.ApplyRoleDelete(f.role.Meta.ID); err != nil {
		t.Fatalf("ApplyRoleDelete: %v", err)
	}
	s := c.Current()
	if _, ok := s.Role(f.role.Meta.ID); ok {
		t.Error("role should be gone")
	}
	for _, b := range []*rolebinding.RoleBinding{f.global, f.teamWide, f.inProject} {
		if _, ok := s.RoleBinding(b.Meta.ID); ok {
			t.Errorf("binding %q should be evicted with its role", b.Meta.Name)
		}
	}
	if got := s.RoleBindingsForSubject("group:platform-eng"); len(got) != 0 {
		t.Errorf("subject index still holds %v", bindingNames(got))
	}
}

func TestReconcile_ProjectDeleteEvictsBindings(t *testing.T) {
	f := newRBACFixture()
	c := f.catalog(t)

	if err := c.ApplyProjectDelete(f.live.Meta.ID); err != nil {
		t.Fatalf("ApplyProjectDelete: %v", err)
	}
	s := c.Current()
	if _, ok := s.RoleBinding(f.inProject.Meta.ID); ok {
		t.Error("project-scoped role binding should be evicted with its project")
	}
	if len(s.PolicyBindingsForProject(f.live.Meta.ID)) != 0 {
		t.Error("policy bindings should be evicted with their project")
	}
	if _, ok := s.RoleBinding(f.teamWide.Meta.ID); !ok {
		t.Error("team-scoped binding should survive a project delete")
	}
}

func TestReconcile_PolicyDeleteEvictsPolicyBindings(t *testing.T) {
	f := newRBACFixture()
	c := f.catalog(t)

	if err := c.ApplyPolicyDelete(f.pol.Meta.ID); err != nil {
		t.Fatalf("ApplyPolicyDelete: %v", err)
	}
	s := c.Current()
	if len(s.PolicyBindingsForProject(f.live.Meta.ID)) != 0 {
		t.Errorf("policy bindings should be evicted with their policy: %v",
			policyBindingNames(s.PolicyBindingsForProject(f.live.Meta.ID)))
	}
	if _, ok := s.RoleBinding(f.inProject.Meta.ID); !ok {
		t.Error("role bindings should survive a policy delete")
	}
}
