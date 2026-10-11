// equivalence_test.go runs the evaluator kept in legacy_test.go and the auth/rbac-backed RBAC over the same snapshots and requires the same answer on every case of a generated matrix.

package authz_test

import (
	"fmt"
	"math/rand"
	"slices"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	coreauthz "github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/rbac"
	"github.com/wyolet/relay/pkg/ids"
)

// eqPlurals are the action kinds the matrix covers: every role-rule kind, catalog and shared catalog kinds among them.
var eqPlurals = []string{
	"providers", "hosts", "models", "host-bindings", "pricings", "rate-limits",
	"policies", "roles", "teams", "projects", "keys", "host-keys",
	"service-accounts", "role-bindings", "policy-bindings", "groups", "users",
	"usage", "logs", "tokens", "settings", "audit", "system",
}

// eqVerbs end the action; "overlay.get" makes a sub-resource action and "" an action with no verb segment at all.
var eqVerbs = []string{"get", "list", "create", "update", "delete", "rotate", "mint", "read", "overlay.get", "*", ""}

type eqActor struct {
	name string
	a    *actor.Actor
}

type eqFixture struct {
	cat                    *catalog.Catalog
	t1, t2                 string
	p1, p2, p3, p4         string
	roles                  []*role.Role
	actors                 []eqActor
	otherUser, unknownTeam string
	unknownProject         string
}

// newEqFixture builds teams T1 (P1, P2) and T2 (P3, disabled P4) with users bound at every scope, through a group and as a service account, plus two wildcard roles. everyone adds a binding on the all-authenticated group.
func newEqFixture(t *testing.T, everyone bool) *eqFixture {
	t.Helper()
	f := &eqFixture{
		t1: ids.New(), t2: ids.New(),
		p1: ids.New(), p2: ids.New(), p3: ids.New(), p4: ids.New(),
		otherUser: ids.New(), unknownTeam: ids.New(), unknownProject: ids.New(),
	}
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
	custom := func(name string, kinds, verbs []string) *role.Role {
		r := &role.Role{Meta: meta.Metadata{ID: ids.New(), Name: name, Owner: globalScope}}
		r.Spec.Rules = []role.Rule{{Kinds: kinds, Verbs: verbs}}
		r.Spec.Enabled = &yes
		roleID[name] = r.Meta.ID
		return r
	}
	f.roles = append(builtins,
		custom("get-everything", []string{"*"}, []string{"get"}),
		custom("models-anything", []string{"models"}, []string{"*"}),
	)

	uProj, uTeam, uGlobal, uWild, uGroup, uMulti, stranger := ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New(), ids.New()
	saID := ids.New()

	eng := &group.Group{}
	eng.Meta = meta.Metadata{ID: ids.New(), Name: "eng", Owner: globalScope}
	eng.Spec.MemberIDs = []string{uGroup}
	eng.Spec.Enabled = &yes

	sa := &serviceaccount.ServiceAccount{}
	sa.Meta = meta.Metadata{ID: saID, Name: "sa", Owner: projectScope(f.p1)}
	sa.Spec.ProjectID = f.p1
	sa.Spec.Enabled = &yes

	bindings := []*rolebinding.RoleBinding{
		mkBinding("proj-dev", roleID["developer"], projectScope(f.p1), userSubject(uProj)),
		mkBinding("proj-dead", roleID["project-admin"], projectScope(f.p4), userSubject(uProj)),
		mkBinding("team-admin", roleID["team-admin"], teamScope(f.t1), userSubject(uTeam)),
		mkBinding("global-editor", roleID["catalog-editor"], globalScope, userSubject(uGlobal)),
		mkBinding("global-auditor", roleID["auditor"], globalScope, userSubject(uGlobal)),
		mkBinding("wild-get", roleID["get-everything"], teamScope(f.t2), userSubject(uWild)),
		mkBinding("wild-models", roleID["models-anything"], globalScope, userSubject(uWild)),
		mkBinding("eng-admin", roleID["project-admin"], projectScope(f.p2), groupSubject("eng")),
		mkBinding("multi-viewer", roleID["viewer"], projectScope(f.p3), userSubject(uMulti)),
		mkBinding("multi-dev", roleID["developer"], projectScope(f.p2), userSubject(uMulti), userSubject(uProj)),
		mkBinding("multi-admin", roleID["admin"], projectScope(f.p1), userSubject(uMulti)),
		mkBinding("sa-dev", roleID["developer"], projectScope(f.p1), saSubject(saID)),
	}
	if everyone {
		bindings = append(bindings,
			mkBinding("everyone", roleID["viewer"], teamScope(f.t2), groupSubject("system:authenticated")))
	}

	f.cat = catalogtest.Catalog{
		Teams: []*team.Team{mkTeam(f.t1, "t1"), mkTeam(f.t2, "t2")},
		Projects: []*project.Project{
			mkProject(f.p1, "p1", f.t1, true), mkProject(f.p2, "p2", f.t1, true),
			mkProject(f.p3, "p3", f.t2, true), mkProject(f.p4, "p4", f.t2, false),
		},
		ServiceAccounts: []*serviceaccount.ServiceAccount{sa},
		Groups:          []*group.Group{eng},
		Roles:           f.roles,
		RoleBindings:    bindings,
	}.Load(t)

	signedIn := func(id string, groups ...string) *actor.Actor { return actorOf(id, subjectsOf(id, groups...)) }
	f.actors = []eqActor{
		{"unauthenticated", nil},
		{"empty actor", &actor.Actor{}},
		{"subjects without a user", &actor.Actor{Subjects: subjectsOf(uProj)}},
		{"admin role without a user", &actor.Actor{Roles: []string{user.RoleAdmin}}},
		{"admin token", &actor.Actor{AdminToken: true}},
		{"admin role", actorOf(ids.New(), nil, user.RoleAdmin)},
		{"project developer", signedIn(uProj)},
		{"team admin", signedIn(uTeam)},
		{"global editor and auditor", signedIn(uGlobal)},
		{"wildcard roles", signedIn(uWild)},
		{"group member", signedIn(uGroup, "eng")},
		{"several bindings", signedIn(uMulti)},
		{"service account", actorOf(saID, []string{"serviceaccount:" + saID, catalog.SubjectServiceAccounts,
			catalog.SubjectServiceAccounts + ":p1", catalog.SubjectAuthenticated})},
		{"stranger", signedIn(stranger)},
	}
	return f
}

// owners returns every owner shape the matrix probes for a, nil first.
func (f *eqFixture) owners(a *actor.Actor) []*meta.Owner {
	out := []*meta.Owner{
		nil, {}, &globalScope,
		{Kind: meta.OwnerHost, ID: "host-1"}, providerOwner(),
		userOwner(f.otherUser), {Kind: meta.OwnerUser},
		{Kind: meta.OwnerTeam, ID: f.t1}, {Kind: meta.OwnerTeam, ID: f.t2}, {Kind: meta.OwnerTeam, ID: f.unknownTeam},
		projectOwner(f.p1), projectOwner(f.p2), projectOwner(f.p3), projectOwner(f.p4), projectOwner(f.unknownProject),
	}
	if a != nil && a.UserID != "" {
		out = append(out, userOwner(a.UserID))
	}
	return out
}

func (f *eqFixture) ids() []string {
	return []string{"", f.t1, f.t2, f.p1, f.p2, f.p4, f.unknownProject}
}

func eqAction(plural, verb string) string {
	if verb == "" {
		return plural
	}
	return plural + "." + verb
}

// resourceKinds is the singular handlers pass for plural, plus the two kinds that are their own scope, so a mismatched kind is covered too.
func resourceKinds(plural string) []string {
	out := []string{authz.Singular(plural)}
	for _, k := range []string{"team", "project"} {
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// compareAuthorize runs both evaluators over the full matrix for one snapshot accessor and returns the number of cases compared.
func compareAuthorize(t *testing.T, f *eqFixture, snap func() authz.Snapshot) int {
	t.Helper()
	legacy := legacyRBAC{Snap: snap}
	current := authz.RBAC{Snap: snap}
	if snap == nil {
		legacy, current = legacyRBAC{}, authz.RBAC{}
	}
	n := 0
	for _, ea := range f.actors {
		ctx := ctxOf(ea.a)
		owners := f.owners(ea.a)
		for _, plural := range eqPlurals {
			for _, verb := range eqVerbs {
				action := eqAction(plural, verb)
				for _, kind := range resourceKinds(plural) {
					for _, id := range f.ids() {
						for _, o := range owners {
							res := authz.Resource{Kind: kind, ID: id, Owner: o}
							want := legacy.Authorize(ctx, action, res)
							got := current.Authorize(ctx, action, res)
							n++
							if want != got {
								t.Fatalf("%s: %s on %s id=%q owner=%+v: legacy %v, rbac %v",
									ea.name, action, kind, id, o, want, got)
							}
						}
					}
				}
			}
		}
		for _, plural := range eqPlurals {
			kind := authz.Singular(plural)
			for _, id := range f.ids() {
				for _, o := range owners[1:] {
					want := legacy.Visible(ctx, kind, id, *o)
					got := current.Visible(ctx, kind, id, *o)
					n++
					if want != got {
						t.Fatalf("%s: Visible(%s, %q, %+v): legacy %v, rbac %v", ea.name, kind, id, *o, want, got)
					}
				}
			}
		}
	}
	return n
}

func TestEquivalenceWithLegacyEvaluator(t *testing.T) {
	total := 0
	for _, everyone := range []bool{false, true} {
		f := newEqFixture(t, everyone)
		current := func() authz.Snapshot { return f.cat.Current() }
		total += compareAuthorize(t, f, current)
		total += compareAuthorize(t, f, nil)
		total += compareAuthorize(t, f, func() authz.Snapshot { return nil })
		total += compareCheckGrant(t, f, current)
		if err := f.cat.ApplyTeamDelete(f.t1); err != nil {
			t.Fatalf("delete team: %v", err)
		}
		total += compareAuthorize(t, f, current)
		total += compareCheckGrant(t, f, current)
	}
	t.Logf("%d cases agree", total)
}

// Pooled evaluations are reused across goroutines: concurrent decisions must not see each other's buffers.
func TestEquivalenceUnderConcurrency(t *testing.T) {
	f := newEqFixture(t, true)
	snap := func() authz.Snapshot { return f.cat.Current() }
	legacy, current := legacyRBAC{Snap: snap}, authz.RBAC{Snap: snap}
	var wg sync.WaitGroup
	for _, ea := range f.actors {
		wg.Go(func() {
			ctx := ctxOf(ea.a)
			for _, plural := range eqPlurals {
				for _, o := range f.owners(ea.a) {
					res := authz.Resource{Kind: authz.Singular(plural), ID: f.p1, Owner: o}
					for _, verb := range []string{"get", "list", "delete"} {
						if want, got := legacy.Authorize(ctx, plural+"."+verb, res), current.Authorize(ctx, plural+"."+verb, res); want != got {
							t.Errorf("%s: %s.%s on %+v: legacy %v, rbac %v", ea.name, plural, verb, o, want, got)
							return
						}
					}
				}
			}
		})
	}
	wg.Wait()
}

// The generated tenancy trees of rbac_property_test.go, through the same comparison.
func TestEquivalenceOverGeneratedWorlds(t *testing.T) {
	total := 0
	for _, seed := range propertySeeds {
		w := generate(t, rand.New(rand.NewSource(seed)))
		snap := func() authz.Snapshot { return w.cat.Current() }
		legacy, current := legacyRBAC{Snap: snap}, authz.RBAC{Snap: snap}
		actors := w.actors()
		actors["admin-token"] = &actor.Actor{AdminToken: true}
		owners := scopedOwners(w, nil, &globalScope, providerOwner(), userOwner(ids.New()))
		for _, u := range w.users {
			owners = append(owners, userOwner(u))
		}
		idsOf := []string{""}
		for _, tm := range w.teams {
			idsOf = append(idsOf, tm.Meta.ID)
		}
		for _, p := range w.rows {
			idsOf = append(idsOf, p.Meta.ID)
		}
		for name, a := range actors {
			ctx := ctxOf(a)
			for _, plural := range eqPlurals {
				for _, verb := range eqVerbs {
					action := eqAction(plural, verb)
					for _, kind := range resourceKinds(plural) {
						for _, id := range idsOf {
							for _, o := range owners {
								res := authz.Resource{Kind: kind, ID: id, Owner: o}
								want := legacy.Authorize(ctx, action, res)
								got := current.Authorize(ctx, action, res)
								total++
								if want != got {
									t.Fatalf("seed %d, %s: %s on %s id=%q owner=%+v: legacy %v, rbac %v",
										seed, name, action, kind, id, o, want, got)
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d cases agree", total)
}

// compareCheckGrant binds every role at every scope as every actor, through both evaluators, and requires the same verdict and message.
func compareCheckGrant(t *testing.T, f *eqFixture, snap func() authz.Snapshot) int {
	t.Helper()
	legacy, current := legacyRBAC{Snap: snap}, authz.RBAC{Snap: snap}
	scopes := []meta.Owner{
		globalScope, teamScope(f.t1), teamScope(f.t2), teamScope(f.unknownTeam),
		projectScope(f.p1), projectScope(f.p2), projectScope(f.p4),
	}
	n := 0
	for _, ea := range f.actors {
		ctx := ctxOf(ea.a)
		for _, r := range f.roles {
			for _, s := range scopes {
				pairs := []struct {
					name        string
					old, gotErr error
				}{
					{"rbac", legacyCheckGrant(ctx, legacy, r, s), authz.CheckGrant(ctx, current, r, s)},
					{"nil authorizer", legacyCheckGrant(ctx, nil, r, s), authz.CheckGrant(ctx, nil, r, s)},
					{"single-user", legacyCheckGrant(ctx, authz.AlwaysAllowAuthenticated{}, r, s),
						authz.CheckGrant(ctx, authz.AlwaysAllowAuthenticated{}, r, s)},
				}
				for _, p := range pairs {
					n++
					if fmt.Sprint(p.old) != fmt.Sprint(p.gotErr) {
						t.Fatalf("%s, %s: CheckGrant(%s at %+v): legacy %v, rbac %v", ea.name, p.name, r.Meta.Name, s, p.old, p.gotErr)
					}
				}
			}
		}
	}
	return n
}

// ScopeOf over relay's rules must select exactly the rows Authorize admits.
func TestScopeOfMatchesAuthorize(t *testing.T) {
	for _, everyone := range []bool{false, true} {
		f := newEqFixture(t, everyone)
		snap := f.cat.Current()
		current := authz.RBAC{Snap: func() authz.Snapshot { return snap }}
		rows := 0
		for _, ea := range f.actors {
			if !ea.a.IsAuthenticated() {
				continue
			}
			ctx := ctxOf(ea.a)
			for _, plural := range eqPlurals {
				kind := authz.Singular(plural)
				for _, verb := range []string{"get", "list", "update", "delete", "read", "mint"} {
					filter, err := authz.ScopeOf(ctx, current, verb, plural)
					if err != nil {
						t.Fatalf("%s: ScopeOf(%s, %s): %v", ea.name, verb, plural, err)
					}
					for _, id := range f.ids()[1:] {
						for _, o := range f.owners(ea.a)[1:] {
							res := authz.Resource{Kind: kind, ID: id, Owner: o}
							want := current.Authorize(ctx, plural+"."+verb, res) == nil
							rows++
							if got := filterMatches(snap, filter, res); got != want {
								t.Fatalf("%s: %s.%s on id=%q owner=%+v: filter %+v selects %v, Authorize %v",
									ea.name, plural, verb, id, *o, filter, got, want)
							}
						}
					}
				}
			}
		}
		t.Logf("%d rows agree", rows)
	}
	if _, err := authz.ScopeOf(ctxOf(nil), authz.RBAC{}, "get", "keys"); err != authz.ErrUnauthenticated {
		t.Fatalf("unauthenticated ScopeOf = %v", err)
	}
}

// filterMatches is the query a store would run for f, against one row.
func filterMatches(snap authz.Snapshot, f rbac.Filter, res authz.Resource) bool {
	if len(f.IDs) > 0 && !slices.Contains(f.IDs, res.ID) {
		return false
	}
	if f.All {
		return true
	}
	for _, o := range snap.ScopeChainFor(res.Kind, res.ID, res.Owner) {
		if slices.Contains(f.Scopes, rbac.Scope{Kind: string(o.Kind), ID: o.ID}) {
			return true
		}
	}
	return slices.Contains(f.Owners, coreauthz.Owner{Kind: string(res.Owner.Kind), ID: res.Owner.ID}) ||
		slices.Contains(f.OwnerKinds, string(res.Owner.Kind))
}
