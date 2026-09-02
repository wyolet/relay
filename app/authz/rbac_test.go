// External test package: app/catalog reaches app/authz through the boot
// seed, so an in-package test importing the catalog would close a cycle.
package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/ids"
)

// lister serves a fixed slice to catalog.New / UseTenancy.
type lister[T any] []*T

func (l lister[T]) List(context.Context) ([]*T, error) { return l, nil }

// Fixture ids: teams T1 (projects P1, P2) and T2 (P3, disabled P4).
var (
	t1ID, t2ID      = ids.New(), ids.New()
	p1ID, p2ID      = ids.New(), ids.New()
	p3ID, p4ID      = ids.New(), ids.New()
	aliceID, bobID  = ids.New(), ids.New()
	carolID, daveID = ids.New(), ids.New()
	erinID, frankID = ids.New(), ids.New()
	ginaID, sa1ID   = ids.New(), ids.New()

	globalScope = meta.Owner{Kind: meta.OwnerSystem}
	yes         = true
	no          = false
)

func projectOwner(id string) *meta.Owner { return &meta.Owner{Kind: meta.OwnerProject, ID: id} }
func userOwner(id string) *meta.Owner    { return &meta.Owner{Kind: meta.OwnerUser, ID: id} }
func providerOwner() *meta.Owner         { return &meta.Owner{Kind: meta.OwnerProvider, ID: "prov-1"} }
func teamScope(id string) meta.Owner     { return meta.Owner{Kind: meta.OwnerTeam, ID: id} }
func projectScope(id string) meta.Owner  { return meta.Owner{Kind: meta.OwnerProject, ID: id} }

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

	cat := catalog.New(
		lister[provider.Provider](nil),
		lister[host.Host](nil),
		lister[policy.Policy](nil),
		lister[model.Model](nil),
		lister[hostkey.HostKey](nil),
		lister[ratelimit.RateLimit](nil),
		lister[key.Key](nil),
		lister[pricing.Pricing](nil),
		lister[binding.Binding](nil),
	)
	cat.UseTenancy(
		lister[team.Team]{mkTeam(t1ID, "t1"), mkTeam(t2ID, "t2")},
		lister[project.Project]{
			mkProject(p1ID, "p1", t1ID, true),
			mkProject(p2ID, "p2", t1ID, true),
			mkProject(p3ID, "p3", t2ID, true),
			mkProject(p4ID, "p4", t2ID, false),
		},
		lister[serviceaccount.ServiceAccount]{sa1},
		lister[group.Group]{ml},
		lister[role.Role](builtins),
		lister[rolebinding.RoleBinding]{
			mkBinding("alice-team-admin", roleID["team-admin"], teamScope(t1ID), userSubject(aliceID)),
			mkBinding("bob-developer", roleID["developer"], projectScope(p1ID), userSubject(bobID)),
			mkBinding("carol-viewer", roleID["viewer"], teamScope(t1ID), userSubject(carolID)),
			mkBinding("dave-catalog-editor", roleID["catalog-editor"], globalScope, userSubject(daveID)),
			mkBinding("erin-auditor", roleID["auditor"], globalScope, userSubject(erinID)),
			mkBinding("ml-project-admin", roleID["project-admin"], projectScope(p2ID), groupSubject("ml")),
			mkBinding("sa1-developer", roleID["developer"], projectScope(p1ID), saSubject(sa1ID)),
			mkBinding("gina-project-admin", roleID["project-admin"], projectScope(p4ID), userSubject(ginaID)),
		},
		lister[policybinding.PolicyBinding](nil),
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return cat
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

// After the team is deleted every grant that hung off it is gone, and the
// rows under it are no longer in the snapshot: cases 3, 5, 8 and 16 flip.
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
		{"8", carol, "policies.list", authz.Resource{Kind: "policy"}},
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

// Invariant 6: the two entry points can never disagree.
func TestVisibleMatchesAuthorizeGet(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}

	actors := []*actor.Actor{
		actorOf(aliceID, subjectsOf(aliceID)),
		actorOf(bobID, subjectsOf(bobID)),
		actorOf(carolID, subjectsOf(carolID)),
		actorOf(erinID, subjectsOf(erinID)),
		actorOf(frankID, subjectsOf(frankID, "ml")),
		{AdminToken: true},
		actorOf(ids.New(), nil, user.RoleAdmin),
	}
	owners := []meta.Owner{
		*projectOwner(p1ID), *projectOwner(p2ID), *projectOwner(p3ID), *projectOwner(p4ID),
		{Kind: meta.OwnerTeam, ID: t1ID}, {Kind: meta.OwnerTeam, ID: t2ID},
		*userOwner(aliceID), *userOwner(bobID), *providerOwner(), globalScope,
	}
	kinds := map[string]string{
		"policy":          "policies",
		"key":             "keys",
		"host-key":        "host-keys",
		"model":           "models",
		"project":         "projects",
		"team":            "teams",
		"service-account": "service-accounts",
		"usage":           "usage",
	}
	for _, a := range actors {
		ctx := ctxOf(a)
		for kind, pl := range kinds {
			for _, o := range owners {
				owner := o
				for _, id := range []string{"", p1ID, t1ID} {
					visible := rbac.Visible(ctx, kind, id, owner)
					allowed := rbac.Authorize(ctx, pl+".get",
						authz.Resource{Kind: kind, ID: id, Owner: &owner}) == nil
					if visible != allowed {
						t.Fatalf("Visible(%s, %s, %+v) = %v but Authorize(%s.get) allowed = %v",
							kind, id, owner, visible, pl, allowed)
					}
				}
			}
		}
	}
}

// The bootstrap admin from config/users carries no bindings at all.
func TestBootstrapAdminNeedsNoBindings(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	root := actorOf(ids.New(), nil, user.RoleAdmin)

	for _, action := range []string{"settings.update", "teams.create", "keys.delete", "system.apply", "audit.read"} {
		if err := rbac.Authorize(ctxOf(root), action, authz.Resource{Kind: "any", Owner: projectOwner(p3ID)}); err != nil {
			t.Fatalf("bootstrap admin denied %q: %v", action, err)
		}
	}
}

// Default deny: an authenticated caller with no bindings gets nothing but
// the catalog reads and their own rows.
func TestDefaultDeny(t *testing.T) {
	cat := newFixture(t)
	rbac := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	stranger := actorOf(ids.New(), subjectsOf(ids.New()))

	if err := rbac.Authorize(ctxOf(stranger), "keys.list", authz.Resource{Kind: "key"}); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("keys.list = %v, want forbidden", err)
	}
	if err := rbac.Authorize(ctxOf(stranger), "models.list", authz.Resource{Kind: "model"}); err != nil {
		t.Fatalf("models.list = %v, want allowed (catalog read)", err)
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
	cat := catalog.New(
		lister[provider.Provider](nil), lister[host.Host](nil), lister[policy.Policy](nil),
		lister[model.Model](nil), lister[hostkey.HostKey](nil), lister[ratelimit.RateLimit](nil),
		lister[key.Key](nil), lister[pricing.Pricing](nil), lister[binding.Binding](nil),
	)
	cat.UseTenancy(
		lister[team.Team](nil), lister[project.Project](nil), lister[serviceaccount.ServiceAccount](nil),
		lister[group.Group](nil),
		lister[role.Role]{rotateRole, updateRole},
		lister[rolebinding.RoleBinding]{
			mkBinding("rotator", rotateRole.Meta.ID, globalScope, userSubject(rotatorID)),
			mkBinding("updater", updateRole.Meta.ID, globalScope, userSubject(updaterID)),
		},
		lister[policybinding.PolicyBinding](nil),
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
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

// Single-user mode: authenticated is the whole check, and unauthenticated
// is still refused.
func TestAlwaysAllowAuthenticated(t *testing.T) {
	a := authz.AlwaysAllowAuthenticated{}
	if err := a.Authorize(ctxOf(nil), "keys.delete", authz.Resource{Kind: "key"}); !errors.Is(err, authz.ErrUnauthenticated) {
		t.Fatalf("unauthenticated = %v, want unauthenticated", err)
	}
	if err := a.Authorize(ctxOf(actorOf(bobID, nil)), "keys.delete",
		authz.Resource{Kind: "key", Owner: userOwner(aliceID)}); err != nil {
		t.Fatalf("authenticated = %v, want allowed", err)
	}
}
