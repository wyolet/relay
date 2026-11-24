// External test package: app/catalog reaches app/authz through the boot
// seed, so an in-package test importing the catalog would close a cycle.

package authz_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// Fixture ids: teams T1 (projects P1, P2) and T2 (P3, disabled P4).
var (
	t1ID, t2ID      = ids.New(), ids.New()
	p1ID, p2ID      = ids.New(), ids.New()
	p3ID, p4ID      = ids.New(), ids.New()
	aliceID, bobID  = ids.New(), ids.New()
	carolID, daveID = ids.New(), ids.New()
	erinID, frankID = ids.New(), ids.New()
	ginaID, sa1ID   = ids.New(), ids.New()
	halID           = ids.New()

	globalScope = meta.Owner{Kind: meta.OwnerSystem}
	yes         = true
	no          = false
)

func projectOwner(id string) *meta.Owner { return &meta.Owner{Kind: meta.OwnerProject, ID: id} }

func userOwner(id string) *meta.Owner { return &meta.Owner{Kind: meta.OwnerUser, ID: id} }

func providerOwner() *meta.Owner { return &meta.Owner{Kind: meta.OwnerProvider, ID: "prov-1"} }

func teamScope(id string) meta.Owner { return meta.Owner{Kind: meta.OwnerTeam, ID: id} }

func projectScope(id string) meta.Owner { return meta.Owner{Kind: meta.OwnerProject, ID: id} }

func mkTeam(id, name string) *team.Team {
	t := &team.Team{}
	t.Meta = meta.Metadata{ID: id, Name: name, Owner: globalScope}
	t.Spec.Enabled = &yes
	return t
}

func mkProject(id, name, teamID string, on bool) *project.Project {
	p := &project.Project{}
	p.Meta = meta.Metadata{ID: id, Name: name, Owner: teamScope(teamID)}
	p.Spec.TeamID = teamID
	if on {
		p.Spec.Enabled = &yes
	} else {
		p.Spec.Enabled = &no
	}
	return p
}

func mkBinding(name, roleID string, scope meta.Owner, subs ...rolebinding.Subject) *rolebinding.RoleBinding {
	b := &rolebinding.RoleBinding{}
	b.Meta = meta.Metadata{ID: ids.New(), Name: name, Owner: scope}
	b.Spec.RoleID = roleID
	b.Spec.Scope = scope
	b.Spec.Subjects = subs
	b.Spec.Enabled = &yes
	return b
}

func userSubject(id string) rolebinding.Subject {
	return rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id}
}

func groupSubject(name string) rolebinding.Subject {
	return rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: name}
}

func saSubject(id string) rolebinding.Subject {
	return rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: id}
}

// newFixture builds the catalog the scope-chain test table describes.
func newFixture(t *testing.T) *catalog.Catalog {
	t.Helper()

	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	roleID := map[string]string{}
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		roleID[r.Meta.Name] = r.Meta.ID
	}

	ml := &group.Group{}
	ml.Meta = meta.Metadata{ID: ids.New(), Name: "ml", Owner: globalScope}
	ml.Spec.MemberIDs = []string{frankID}
	ml.Spec.Enabled = &yes

	sa1 := &serviceaccount.ServiceAccount{}
	sa1.Meta = meta.Metadata{ID: sa1ID, Name: "sa1", Owner: projectScope(p1ID)}
	sa1.Spec.ProjectID = p1ID
	sa1.Spec.Enabled = &yes

	return catalogtest.Catalog{
		Teams: []*team.Team{mkTeam(t1ID, "t1"), mkTeam(t2ID, "t2")},
		Projects: []*project.Project{
			mkProject(p1ID, "p1", t1ID, true),
			mkProject(p2ID, "p2", t1ID, true),
			mkProject(p3ID, "p3", t2ID, true),
			mkProject(p4ID, "p4", t2ID, false),
		},
		ServiceAccounts: []*serviceaccount.ServiceAccount{sa1},
		Groups:          []*group.Group{ml},
		Roles:           builtins,
		RoleBindings: []*rolebinding.RoleBinding{
			mkBinding("alice-team-admin", roleID["team-admin"], teamScope(t1ID), userSubject(aliceID)),
			mkBinding("bob-developer", roleID["developer"], projectScope(p1ID), userSubject(bobID)),
			mkBinding("carol-viewer", roleID["viewer"], teamScope(t1ID), userSubject(carolID)),
			mkBinding("dave-catalog-editor", roleID["catalog-editor"], globalScope, userSubject(daveID)),
			mkBinding("erin-auditor", roleID["auditor"], globalScope, userSubject(erinID)),
			mkBinding("ml-project-admin", roleID["project-admin"], projectScope(p2ID), groupSubject("ml")),
			mkBinding("sa1-developer", roleID["developer"], projectScope(p1ID), saSubject(sa1ID)),
			mkBinding("gina-project-admin", roleID["project-admin"], projectScope(p4ID), userSubject(ginaID)),
			mkBinding("hal-global-developer", roleID["developer"], globalScope, userSubject(halID)),
		},
	}.Load(t)
}

// Actors. Subjects are what the session middleware builds; a service
// account never holds a control-plane session, so sa1 stands in with its
// own id as the authenticated principal.
func actorOf(id string, subjects []string, roles ...string) *actor.Actor {
	return &actor.Actor{UserID: id, Subjects: subjects, Roles: roles}
}

func subjectsOf(id string, groups ...string) []string {
	return catalog.UserSubjects(id, groups, nil)
}

func ctxOf(a *actor.Actor) context.Context {
	if a == nil {
		return context.Background()
	}
	return actor.WithActor(context.Background(), a)
}

func TestRBACTable(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	alice := actorOf(aliceID, subjectsOf(aliceID))
	bob := actorOf(bobID, subjectsOf(bobID))
	carol := actorOf(carolID, subjectsOf(carolID))
	dave := actorOf(daveID, subjectsOf(daveID))
	erin := actorOf(erinID, subjectsOf(erinID))
	frank := actorOf(frankID, subjectsOf(frankID, "ml"))
	gina := actorOf(ginaID, subjectsOf(ginaID))
	sa1 := actorOf(sa1ID, []string{"serviceaccount:" + sa1ID, catalog.SubjectServiceAccounts, catalog.SubjectAuthenticated})
	adminToken := &actor.Actor{AdminToken: true}

	tests := []struct {
		name   string
		who    *actor.Actor
		action string
		res    authz.Resource
		want   error
	}{
		{"1 unauthenticated", nil, "policies.list", authz.Resource{Kind: "policy"}, authz.ErrUnauthenticated},
		{"2 admin token", adminToken, "settings.update", authz.Resource{Kind: "settings"}, nil},
		{"3 team scope reaches every project", alice, "keys.delete", authz.Resource{Kind: "key", Owner: projectOwner(p2ID)}, nil},
		{"4 other team", alice, "keys.delete", authz.Resource{Kind: "key", Owner: projectOwner(p3ID)}, authz.ErrForbidden},
		{"5 project binding on its own project", bob, "keys.create", authz.Resource{Kind: "key", Owner: projectOwner(p1ID)}, nil},
		{"6 project binding does not leak to a sibling", bob, "keys.get", authz.Resource{Kind: "key", Owner: projectOwner(p2ID)}, authz.ErrForbidden},
		{"7 developer cannot mutate policies", bob, "policies.update", authz.Resource{Kind: "policy", Owner: projectOwner(p1ID)}, authz.ErrForbidden},
		{"8 list with a granting binding anywhere", carol, "policies.list", authz.Resource{Kind: "policy"}, nil},
		{"10 global auditor reads usage", erin, "usage.read", authz.Resource{Kind: "usage"}, nil},
		{"11 project actor cannot read all usage", bob, "usage.read", authz.Resource{Kind: "usage"}, authz.ErrForbidden},
		{"12 personal row", bob, "host-keys.get", authz.Resource{Kind: "host-key", Owner: userOwner(bobID)}, nil},
		{"13 someone else's personal row", bob, "host-keys.update", authz.Resource{Kind: "host-key", Owner: userOwner(aliceID)}, authz.ErrForbidden},
		{"14 catalog read", bob, "models.get", authz.Resource{Kind: "model", Owner: providerOwner()}, nil},
		{"15 catalog mutation needs the editor", bob, "models.update", authz.Resource{Kind: "model", Owner: providerOwner()}, authz.ErrForbidden},
		{"15 catalog editor mutates", dave, "models.update", authz.Resource{Kind: "model", Owner: providerOwner()}, nil},
		{"16 group subject", frank, "service-accounts.delete", authz.Resource{Kind: "service-account", Owner: projectOwner(p2ID)}, nil},
		{"17 disabled project collapses to global", gina, "keys.get", authz.Resource{Kind: "key", Owner: projectOwner(p4ID)}, authz.ErrForbidden},
		{"18 a project row lives in its team", alice, "projects.update", authz.Resource{Kind: "project", Owner: &meta.Owner{Kind: meta.OwnerTeam, ID: t2ID}}, authz.ErrForbidden},
		{"19 create without an owner fails closed", bob, "policies.create", authz.Resource{Kind: "policy"}, authz.ErrForbidden},
		{"20 mint is granted at a project, not globally", sa1, "tokens.mint", authz.Resource{Kind: "token"}, authz.ErrForbidden},
		{"21 a project developer cannot create a system-owned group", bob, "groups.create", authz.Resource{Kind: "group", Owner: &globalScope}, authz.ErrForbidden},
		{"22 admin creates a system-owned group", adminToken, "groups.create", authz.Resource{Kind: "group", Owner: &globalScope}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rbac.Authorize(ctxOf(tt.who), tt.action, tt.res)
			if !errors.Is(got, tt.want) {
				t.Fatalf("Authorize(%q) = %v, want %v", tt.action, got, tt.want)
			}
		})
	}

	// Case 9 and the Visible half of case 11.
	if rbac.Visible(ctxOf(carol), "policy", "", *projectOwner(p3ID)) {
		t.Error("carol sees a policy in another team's project")
	}
	if !rbac.Visible(ctxOf(carol), "policy", "", *projectOwner(p1ID)) {
		t.Error("carol cannot see a policy in her own team")
	}
	if !rbac.Visible(ctxOf(bob), "usage", "", *projectOwner(p1ID)) {
		t.Error("bob cannot see his own project's usage")
	}
	if rbac.Visible(ctxOf(bob), "usage", "", *projectOwner(p2ID)) {
		t.Error("bob sees a sibling project's usage")
	}
}

// A user's service account, its account and its keys are the user's personal rows: the user holds every verb on them, and no other user does through a team or project role, the team admin included; the admin token and a global admin do. A role at the global scope reaches them as it reaches every personal row, but minting or rotating a key of the service account stays with its user and an admin.
func TestRBACUserServiceAccountRows(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	bobs := userOwner(bobID)
	bob := actorOf(bobID, subjectsOf(bobID))
	others := map[string]*actor.Actor{
		"a team admin":   actorOf(aliceID, subjectsOf(aliceID)),
		"a team viewer":  actorOf(carolID, subjectsOf(carolID)),
		"a group admin":  actorOf(frankID, subjectsOf(frankID, "ml")),
		"a stranger":     actorOf(ids.New(), subjectsOf(ids.New())),
		"a service acct": actorOf(sa1ID, []string{"serviceaccount:" + sa1ID, catalog.SubjectServiceAccounts, catalog.SubjectAuthenticated}),
	}
	for _, kind := range []string{"service-accounts", "accounts", "keys"} {
		res := authz.Resource{Kind: strings.TrimSuffix(kind, "s"), Owner: bobs}
		for _, verb := range []string{"get", "list", "create", "update", "delete"} {
			if err := rbac.Authorize(ctxOf(bob), kind+"."+verb, res); err != nil {
				t.Errorf("owner %s.%s = %v, want allowed", kind, verb, err)
			}
			for name, who := range others {
				if err := rbac.Authorize(ctxOf(who), kind+"."+verb, res); !errors.Is(err, authz.ErrForbidden) {
					t.Errorf("%s %s.%s = %v, want forbidden", name, kind, verb, err)
				}
			}
			for name, who := range map[string]*actor.Actor{"the admin token": {AdminToken: true}, "an admin": actorOf(ids.New(), nil, user.RoleAdmin)} {
				if err := rbac.Authorize(ctxOf(who), kind+"."+verb, res); err != nil {
					t.Errorf("%s %s.%s = %v, want allowed", name, kind, verb, err)
				}
			}
		}
		if rbac.Visible(ctxOf(others["a team admin"]), strings.TrimSuffix(kind, "s"), "", *bobs) {
			t.Errorf("a team admin sees a %s of another user", kind)
		}
	}

	hal := actorOf(halID, subjectsOf(halID))
	for _, tc := range []struct {
		action string
		want   error
	}{
		{"service-accounts.get", nil}, {"service-accounts.update", nil}, {"service-accounts.delete", nil},
		{"keys.get", nil}, {"keys.create", nil}, {"keys.rotate", nil}, {"keys.delete", nil},
		{"accounts.get", nil}, {"accounts.update", authz.ErrForbidden},
	} {
		kind := strings.TrimSuffix(strings.Split(tc.action, ".")[0], "s")
		if err := rbac.Authorize(ctxOf(hal), tc.action, authz.Resource{Kind: kind, Owner: bobs}); !errors.Is(err, tc.want) {
			t.Errorf("a global developer %s = %v, want %v", tc.action, err, tc.want)
		}
	}
	mint := refcheck.Checker{Authz: rbac}
	for name, who := range map[string]*actor.Actor{"a global developer": hal, "a team admin": others["a team admin"]} {
		if err := mint.UserServiceAccountKey(ctxOf(who), *bobs); !errors.Is(err, authz.ErrForbidden) {
			t.Errorf("%s mints a key of bob's service account: %v, want forbidden", name, err)
		}
	}
	for name, who := range map[string]*actor.Actor{"bob": bob, "the admin token": {AdminToken: true}, "an admin": actorOf(ids.New(), nil, user.RoleAdmin)} {
		if err := mint.UserServiceAccountKey(ctxOf(who), *bobs); err != nil {
			t.Errorf("%s mints a key of bob's service account: %v, want allowed", name, err)
		}
	}
}

// After the team is deleted every grant that hung off it is gone, and the
// rows under it are no longer in the snapshot: cases 3, 5 and 16 flip. The
// list call itself stays open — it is the rows it returns that are gone.
func TestRBACAfterTeamDelete(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	if err := cat.ApplyTeamDelete(t1ID); err != nil {
		t.Fatalf("delete team: %v", err)
	}

	alice := actorOf(aliceID, subjectsOf(aliceID))
	bob := actorOf(bobID, subjectsOf(bobID))
	carol := actorOf(carolID, subjectsOf(carolID))
	frank := actorOf(frankID, subjectsOf(frankID, "ml"))

	flipped := []struct {
		name   string
		who    *actor.Actor
		action string
		res    authz.Resource
	}{
		{"3", alice, "keys.delete", authz.Resource{Kind: "key", Owner: projectOwner(p2ID)}},
		{"5", bob, "keys.create", authz.Resource{Kind: "key", Owner: projectOwner(p1ID)}},
		{"16", frank, "service-accounts.delete", authz.Resource{Kind: "service-account", Owner: projectOwner(p2ID)}},
	}
	for _, tt := range flipped {
		t.Run("case "+tt.name, func(t *testing.T) {
			if err := rbac.Authorize(ctxOf(tt.who), tt.action, tt.res); !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("Authorize(%q) = %v, want forbidden after the team delete", tt.action, err)
			}
		})
	}
	if rbac.Visible(ctxOf(carol), "policy", "", *projectOwner(p1ID)) {
		t.Error("carol still sees a policy under the deleted team")
	}
}

// The break-glass token and the bootstrap admin from config/users carry no
// bindings at all, and answer every verb on every owner.
func TestAdminActorsNeedNoBindings(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	owners := []*meta.Owner{
		projectOwner(p1ID), projectOwner(p3ID), projectOwner(p4ID),
		{Kind: meta.OwnerTeam, ID: t2ID}, &globalScope, providerOwner(), userOwner(ids.New()),
	}

	for name, a := range map[string]*actor.Actor{
		"admin token":     {AdminToken: true},
		"bootstrap admin": actorOf(ids.New(), nil, user.RoleAdmin),
	} {
		ctx := ctxOf(a)
		for _, action := range []string{"settings.update", "teams.create", "keys.delete", "system.apply", "audit.read"} {
			if err := rbac.Authorize(ctx, action, authz.Resource{Kind: "any", Owner: projectOwner(p3ID)}); err != nil {
				t.Fatalf("%s denied %q: %v", name, action, err)
			}
		}
		for _, p := range probes {
			for _, o := range owners {
				if err := rbac.Authorize(ctx, p.kind+"."+p.verb, resourceOf(p, o)); err != nil {
					t.Fatalf("%s denied %s.%s on %+v: %v", name, p.kind, p.verb, *o, err)
				}
			}
		}
	}
}

// A Team and a Project are the scope they define: a binding at that scope
// reaches the row itself, and nothing outside it does.
func TestScopeDefiningRowsAreInTheirOwnScope(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	alice := actorOf(aliceID, subjectsOf(aliceID))       // team-admin on T1
	frank := actorOf(frankID, subjectsOf(frankID, "ml")) // project-admin on P2

	// A Team row is system-owned; only its own scope reaches it.
	owner := globalScope
	if !rbac.Visible(ctxOf(alice), "team", t1ID, owner) {
		t.Error("team-admin cannot see the team they administer")
	}
	if rbac.Visible(ctxOf(alice), "team", t2ID, owner) {
		t.Error("team-admin sees a sibling team")
	}
	if err := rbac.Authorize(ctxOf(alice), "teams.update",
		authz.Resource{Kind: "team", ID: t1ID, Owner: &owner}); err != nil {
		t.Errorf("team-admin cannot update their own team: %v", err)
	}

	// A Project row is team-owned; its own scope and its team both reach it.
	inT1 := meta.Owner{Kind: meta.OwnerTeam, ID: t1ID}
	if !rbac.Visible(ctxOf(frank), "project", p2ID, inT1) {
		t.Error("project-admin cannot see the project they administer")
	}
	if rbac.Visible(ctxOf(frank), "project", p1ID, inT1) {
		t.Error("project-admin sees a sibling project")
	}
	for _, verb := range []string{"update", "delete"} {
		if err := rbac.Authorize(ctxOf(frank), "projects."+verb,
			authz.Resource{Kind: "project", ID: p2ID, Owner: &inT1}); err != nil {
			t.Errorf("project-admin cannot %s their own project: %v", verb, err)
		}
	}
	if !rbac.Visible(ctxOf(alice), "project", p2ID, inT1) {
		t.Error("team-admin cannot see a project of their team")
	}
}

// Rotate is authorized as its own verb, distinct from update: a role
// granting one must not imply the other.
func TestRBACRotateIsNotUpdate(t *testing.T) {
	rotateRole := &role.Role{Meta: meta.Metadata{ID: ids.New(), Name: "key-rotator", Owner: globalScope}}
	rotateRole.Spec.Rules = []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"rotate"}}}
	rotateRole.Spec.Enabled = &yes
	updateRole := &role.Role{Meta: meta.Metadata{ID: ids.New(), Name: "key-updater", Owner: globalScope}}
	updateRole.Spec.Rules = []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"update"}}}
	updateRole.Spec.Enabled = &yes

	rotatorID, updaterID := ids.New(), ids.New()
	cat := catalogtest.Catalog{
		Roles: []*role.Role{rotateRole, updateRole},
		RoleBindings: []*rolebinding.RoleBinding{
			mkBinding("rotator", rotateRole.Meta.ID, globalScope, userSubject(rotatorID)),
			mkBinding("updater", updateRole.Meta.ID, globalScope, userSubject(updaterID)),
		},
	}.Load(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	rotator := actorOf(rotatorID, subjectsOf(rotatorID))
	updater := actorOf(updaterID, subjectsOf(updaterID))
	res := authz.Resource{Kind: "key", Owner: &globalScope}

	if err := rbac.Authorize(ctxOf(rotator), "keys.rotate", res); err != nil {
		t.Errorf("rotate-only role denied keys.rotate: %v", err)
	}
	if err := rbac.Authorize(ctxOf(updater), "keys.rotate", res); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("keys.rotate with an update-only role = %v, want forbidden", err)
	}
	if err := rbac.Authorize(ctxOf(rotator), "keys.update", res); !errors.Is(err, authz.ErrForbidden) {
		t.Errorf("keys.update with a rotate-only role = %v, want forbidden", err)
	}
}

// A misconfigured evaluator must fail closed: no snapshot means no
// bindings to read, which is a denial and never a bypass.
func TestRBACWithoutASnapshotDenies(t *testing.T) {
	bob := actorOf(bobID, subjectsOf(bobID))
	res := authz.Resource{Kind: "key", Owner: projectOwner(p1ID)}

	for _, tt := range []struct {
		name string
		rbac authz.RBAC
	}{
		{"no snapshot accessor", authz.RBAC{}},
		{"accessor returns nil", authz.RBAC{Snap: func() authz.Snapshot { return nil }}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.rbac.Authorize(ctxOf(bob), "keys.get", res); !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("Authorize = %v, want forbidden", err)
			}
			// An unauthenticated caller is still refused first, and the
			// admin token still bypasses without reading a snapshot.
			if err := tt.rbac.Authorize(ctxOf(nil), "keys.get", res); !errors.Is(err, authz.ErrUnauthenticated) {
				t.Errorf("unauthenticated = %v, want unauthenticated", err)
			}
			if err := tt.rbac.Authorize(ctxOf(&actor.Actor{AdminToken: true}), "keys.get", res); err != nil {
				t.Errorf("admin token = %v, want allowed", err)
			}
		})
	}
}

// IsAdmin is the operator escape hatch a few call sites read directly; it
// must answer for the break-glass token and the bootstrap admin role only.
func TestIsAdmin(t *testing.T) {
	for _, tt := range []struct {
		name string
		who  *actor.Actor
		want bool
	}{
		{"no actor", nil, false},
		{"plain user", actorOf(bobID, subjectsOf(bobID)), false},
		{"admin token", &actor.Actor{AdminToken: true}, true},
		{"bootstrap admin role", actorOf(ids.New(), nil, user.RoleAdmin), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := authz.IsAdmin(ctxOf(tt.who)); got != tt.want {
				t.Fatalf("IsAdmin = %v, want %v", got, tt.want)
			}
		})
	}
}
