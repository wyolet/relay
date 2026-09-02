// rbac_property_test.go drives the evaluator with randomly generated
// tenancy trees instead of the hand-written table in rbac_test.go. The
// generator is a seeded math/rand loop (the app/catalog/property_test.go
// style) so a failure reproduces from its seed alone.
package authz_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
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
	"github.com/wyolet/relay/pkg/ids"
)

// probe is one (kind, verb) pair the properties evaluate. Kinds are the
// role-rule plurals; the Resource carries the singular handlers stamp.
type probe struct{ kind, verb string }

var probes = []probe{
	{"keys", "get"}, {"keys", "create"}, {"keys", "delete"},
	{"policies", "get"}, {"policies", "update"},
	{"service-accounts", "delete"},
	{"projects", "get"}, {"projects", "update"},
	{"teams", "get"},
	{"role-bindings", "create"},
	{"tokens", "mint"},
	{"usage", "read"},
	{"models", "update"},
}

// world is one generated deployment plus the reference view of it the
// properties reason against.
type world struct {
	teams    []*team.Team
	projects map[string][]*project.Project // by team id, enabled and disabled
	rows     []*project.Project            // the project lister's backing slice
	users    []string
	groupsOf map[string][]string // user id → group names
	roles    []*role.Role
	roleByID map[string]*role.Role
	bindings []*rolebinding.RoleBinding
	cat      *catalog.Catalog
}

// projectRows reads through to a slice the test still owns: upserting a
// project the snapshot has never seen sends the reconciler through a full
// reload, which would drop it again from a fixed lister.
type projectRows struct{ rows *[]*project.Project }

func (l projectRows) List(context.Context) ([]*project.Project, error) { return *l.rows, nil }

// generate builds a random tenancy tree: 2–4 teams with 0–4 projects each
// (a quarter of them disabled), 1–3 users, 0–2 groups, one service account
// per enabled project, the built-in roles, and 1–6 role bindings at random
// scopes. A world with no subject and no binding would prove nothing, so
// both counts start at one; the binding-less case is covered inside every
// world by the stranger actor. Teams are always enabled; a disabled *project* is what exercises
// the collapse-to-global rule, and disabling a team as well would only
// duplicate it one level up.
func generate(t *testing.T, r *rand.Rand) *world {
	t.Helper()

	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	w := &world{
		projects: map[string][]*project.Project{},
		groupsOf: map[string][]string{},
		roleByID: map[string]*role.Role{},
		roles:    builtins,
	}
	for _, ro := range builtins {
		ro.Meta.ID = ids.New()
		ro.Spec.Enabled = &yes
		w.roleByID[ro.Meta.ID] = ro
	}

	var sas []*serviceaccount.ServiceAccount
	for i := 0; i < 2+r.Intn(3); i++ {
		tm := mkTeam(ids.New(), fmt.Sprintf("t%d", i))
		w.teams = append(w.teams, tm)
		for j := 0; j < r.Intn(5); j++ {
			on := r.Intn(4) != 0
			p := mkProject(ids.New(), fmt.Sprintf("t%d-p%d", i, j), tm.Meta.ID, on)
			w.projects[tm.Meta.ID] = append(w.projects[tm.Meta.ID], p)
			w.rows = append(w.rows, p)
			if !on {
				continue
			}
			sa := &serviceaccount.ServiceAccount{}
			sa.Meta = meta.Metadata{ID: ids.New(), Name: fmt.Sprintf("sa-t%d-p%d", i, j),
				Owner: projectScope(p.Meta.ID)}
			sa.Spec.ProjectID = p.Meta.ID
			sa.Spec.Enabled = &yes
			sas = append(sas, sa)
		}
	}

	for i := 0; i < 1+r.Intn(3); i++ {
		w.users = append(w.users, ids.New())
	}
	var groups []*group.Group
	for i := 0; i < r.Intn(3); i++ {
		g := &group.Group{}
		g.Meta = meta.Metadata{ID: ids.New(), Name: fmt.Sprintf("g%d", i), Owner: globalScope}
		g.Spec.Enabled = &yes
		for _, u := range w.users {
			if r.Intn(2) == 0 {
				g.Spec.MemberIDs = append(g.Spec.MemberIDs, u)
				w.groupsOf[u] = append(w.groupsOf[u], g.Meta.Name)
			}
		}
		groups = append(groups, g)
	}

	for i := 0; i < 1+r.Intn(6); i++ {
		scope := globalScope
		switch r.Intn(4) {
		case 1, 2:
			if len(w.rows) > 0 {
				scope = projectScope(w.rows[r.Intn(len(w.rows))].Meta.ID)
			}
		case 3:
			scope = teamScope(w.teams[r.Intn(len(w.teams))].Meta.ID)
		}
		ro := builtins[r.Intn(len(builtins))]
		var sub rolebinding.Subject
		switch {
		case len(groups) > 0 && r.Intn(3) == 0:
			sub = groupSubject(groups[r.Intn(len(groups))].Meta.Name)
		case len(sas) > 0 && r.Intn(4) == 0:
			sub = saSubject(sas[r.Intn(len(sas))].Meta.ID)
		case len(w.users) > 0:
			sub = userSubject(w.users[r.Intn(len(w.users))])
		default:
			continue
		}
		w.bindings = append(w.bindings, mkBinding(fmt.Sprintf("b%d", i), ro.Meta.ID, scope, sub))
	}

	w.cat = newCatalog(t, w.teams, projectRows{&w.rows}, sas, groups, builtins, w.bindings)
	return w
}

// newCatalog boots a Catalog over the tenancy rows alone; no property here
// reads a routing entity.
func newCatalog(t *testing.T, teams []*team.Team, projects catalog.ProjectLister,
	sas []*serviceaccount.ServiceAccount, groups []*group.Group,
	roles []*role.Role, bindings []*rolebinding.RoleBinding,
) *catalog.Catalog {
	t.Helper()
	cat := catalog.New(
		lister[provider.Provider](nil), lister[host.Host](nil), lister[policy.Policy](nil),
		lister[model.Model](nil), lister[hostkey.HostKey](nil), lister[ratelimit.RateLimit](nil),
		lister[key.Key](nil), lister[pricing.Pricing](nil), lister[binding.Binding](nil),
	)
	cat.UseTenancy(
		lister[team.Team](teams), projects,
		lister[serviceaccount.ServiceAccount](sas), lister[group.Group](groups),
		lister[role.Role](roles), lister[rolebinding.RoleBinding](bindings),
		lister[policybinding.PolicyBinding](nil),
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return cat
}

func (w *world) rbac() authz.RBAC {
	return authz.RBAC{Snap: func() authz.Snapshot { return w.cat.Current() }}
}

// actors returns one actor per generated user plus a stranger holding no
// binding at all, so every property also covers the empty-grant case.
func (w *world) actors() map[string]*actor.Actor {
	out := map[string]*actor.Actor{}
	for _, u := range w.users {
		out[u] = actorOf(u, subjectsOf(u, w.groupsOf[u]...))
	}
	stranger := ids.New()
	out[stranger] = actorOf(stranger, subjectsOf(stranger))
	return out
}

func (w *world) teamOf(projectID string) string {
	for tid, ps := range w.projects {
		for _, p := range ps {
			if p.Meta.ID == projectID {
				return tid
			}
		}
	}
	return ""
}

func (w *world) enabled(projectID string) bool {
	for _, ps := range w.projects {
		for _, p := range ps {
			if p.Meta.ID == projectID {
				return p.Spec.Enabled == nil || *p.Spec.Enabled
			}
		}
	}
	return false
}

// chainOf is the reference scope chain: the row's own scope, its team, and
// global — collapsing to [global] whenever the scope row is not in the
// snapshot (a disabled or unknown project).
func (w *world) chainOf(o *meta.Owner) []meta.Owner {
	if o == nil {
		return []meta.Owner{globalScope}
	}
	switch o.Kind {
	case meta.OwnerProject:
		if w.enabled(o.ID) {
			return []meta.Owner{*o, teamScope(w.teamOf(o.ID)), globalScope}
		}
	case meta.OwnerTeam:
		for _, tm := range w.teams {
			if tm.Meta.ID == o.ID {
				return []meta.Owner{*o, globalScope}
			}
		}
	}
	return []meta.Owner{globalScope}
}

// live reports whether a binding survived the build: a binding whose scope
// row is gone (a disabled project) is dropped, not re-homed at global.
func (w *world) live(b *rolebinding.RoleBinding) bool {
	switch b.Spec.Scope.Kind {
	case meta.OwnerProject:
		return w.enabled(b.Spec.Scope.ID)
	case meta.OwnerTeam:
		for _, tm := range w.teams {
			if tm.Meta.ID == b.Spec.Scope.ID {
				return true
			}
		}
		return false
	}
	return true
}

// grants is the reference binding scan: any live binding naming one of the
// actor's subjects, at a scope inside chain, whose role allows kind/verb.
func (w *world) grants(a *actor.Actor, chain []meta.Owner, p probe) bool {
	for _, b := range w.bindings {
		if !w.live(b) || !inChain(chain, b.Spec.Scope) || !w.names(a, b) {
			continue
		}
		if w.roleByID[b.Spec.RoleID].Allows(p.kind, p.verb) {
			return true
		}
	}
	return false
}

func (w *world) names(a *actor.Actor, b *rolebinding.RoleBinding) bool {
	for _, sub := range b.Spec.Subjects {
		for _, s := range a.Subjects {
			if sub.Key() == s {
				return true
			}
		}
	}
	return false
}

func inChain(chain []meta.Owner, scope meta.Owner) bool {
	for _, o := range chain {
		if o.Kind == scope.Kind && o.ID == scope.ID {
			return true
		}
	}
	return false
}

func resourceOf(p probe, owner *meta.Owner) authz.Resource {
	return authz.Resource{Kind: authz.Singular(p.kind), Owner: owner}
}

// scopedOwners lists every project and team owner in the tree; extra
// appends the non-tenancy owners a given property also probes.
func scopedOwners(w *world, extra ...*meta.Owner) []*meta.Owner {
	var out []*meta.Owner
	for _, ps := range w.projects {
		for _, p := range ps {
			out = append(out, projectOwner(p.Meta.ID))
		}
	}
	for _, tm := range w.teams {
		o := teamScope(tm.Meta.ID)
		out = append(out, &o)
	}
	return append(out, extra...)
}

var propertySeeds = []int64{1, 3, 7, 11, 42, 99, 256, 1337, 9999, 20260902}

func forEachWorld(t *testing.T, f func(t *testing.T, w *world)) {
	t.Helper()
	for _, seed := range propertySeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			f(t, generate(t, rand.New(rand.NewSource(seed))))
		})
	}
}

// An actor holding no binding is refused every scoped verb, on every owner
// the generated tree contains.
func TestProperty_DefaultDeny(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		id := ids.New()
		stranger := ctxOf(actorOf(id, subjectsOf(id)))

		owners := scopedOwners(w)
		for _, p := range probes {
			if p.kind == "models" || p.kind == "usage" {
				continue // catalog reads and nil-owner reads have their own properties
			}
			for _, o := range owners {
				if err := rbac.Authorize(stranger, p.kind+"."+p.verb, resourceOf(p, o)); !errors.Is(err, authz.ErrForbidden) {
					t.Fatalf("%s.%s on %+v = %v, want forbidden with no bindings", p.kind, p.verb, *o, err)
				}
			}
		}
	})
}

// A binding at one project never reaches a sibling project of the same
// team: the only way a sibling is allowed is another binding of the same
// actor that legitimately covers it.
func TestProperty_ProjectBindingNeverGrantsOnASibling(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()

		for _, b := range w.bindings {
			if b.Spec.Scope.Kind != meta.OwnerProject || !w.live(b) {
				continue
			}
			tid := w.teamOf(b.Spec.Scope.ID)
			for _, sib := range w.projects[tid] {
				if sib.Meta.ID == b.Spec.Scope.ID {
					continue
				}
				owner := projectOwner(sib.Meta.ID)
				chain := w.chainOf(owner)
				for _, a := range actors {
					if !w.names(a, b) {
						continue
					}
					for _, p := range probes {
						if w.grants(a, chain, p) {
							continue // some other binding covers the sibling
						}
						err := rbac.Authorize(ctxOf(a), p.kind+"."+p.verb, resourceOf(p, owner))
						if !errors.Is(err, authz.ErrForbidden) {
							t.Fatalf("%s.%s leaked from project %s to sibling %s: %v",
								p.kind, p.verb, b.Spec.Scope.ID, sib.Meta.ID, err)
						}
					}
				}
			}
		}
	})
}

// A binding at team scope covers every enabled project of that team, and
// keeps covering one created after the binding was written.
func TestProperty_TeamBindingCoversCurrentAndFutureProjects(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()

		check := func(b *rolebinding.RoleBinding, projectID string) {
			t.Helper()
			owner := projectOwner(projectID)
			for _, a := range actors {
				if !w.names(a, b) {
					continue
				}
				for _, p := range probes {
					if !w.roleByID[b.Spec.RoleID].Allows(p.kind, p.verb) {
						continue
					}
					if err := rbac.Authorize(ctxOf(a), p.kind+"."+p.verb, resourceOf(p, owner)); err != nil {
						t.Fatalf("team binding %s does not reach project %s for %s.%s: %v",
							b.Meta.Name, projectID, p.kind, p.verb, err)
					}
				}
			}
		}

		for _, b := range w.bindings {
			if b.Spec.Scope.Kind != meta.OwnerTeam {
				continue
			}
			for _, p := range w.projects[b.Spec.Scope.ID] {
				if p.Spec.Enabled != nil && !*p.Spec.Enabled {
					continue
				}
				check(b, p.Meta.ID)
			}

			// A project created after the binding inherits it too.
			fresh := mkProject(ids.New(), "later-"+b.Meta.Name, b.Spec.Scope.ID, true)
			w.rows = append(w.rows, fresh)
			if err := w.cat.ApplyProjectUpsert(fresh); err != nil {
				t.Fatalf("ApplyProjectUpsert: %v", err)
			}
			w.projects[b.Spec.Scope.ID] = append(w.projects[b.Spec.Scope.ID], fresh)
			check(b, fresh.Meta.ID)
		}
	})
}

// Invariant 6 over the generated tree: the two entry points can never
// disagree, whatever the shape of the bindings.
func TestProperty_VisibleMatchesAuthorizeGet(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()
		actors["admin-token"] = &actor.Actor{AdminToken: true}

		owners := scopedOwners(w, &globalScope, providerOwner(), userOwner(ids.New()))
		for _, u := range w.users {
			owners = append(owners, userOwner(u))
		}

		for _, a := range actors {
			ctx := ctxOf(a)
			for _, p := range probes {
				kind := authz.Singular(p.kind)
				for _, o := range owners {
					owner := *o
					visible := rbac.Visible(ctx, kind, "", owner)
					allowed := rbac.Authorize(ctx, p.kind+".get",
						authz.Resource{Kind: kind, Owner: &owner}) == nil
					if visible != allowed {
						t.Fatalf("Visible(%s, %+v) = %v but Authorize(%s.get) allowed = %v",
							kind, owner, visible, p.kind, allowed)
					}
				}
			}
		}
	})
}

// A personal row answers every verb to its owner and — absent a binding at
// the global scope its chain collapses to — to nobody else.
func TestProperty_PersonalRowsAreOwnerOnly(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()

		for _, owner := range w.users {
			o := userOwner(owner)
			for _, a := range actors {
				for _, p := range probes {
					err := rbac.Authorize(ctxOf(a), p.kind+"."+p.verb, resourceOf(p, o))
					switch {
					case a.UserID == owner:
						if err != nil {
							t.Fatalf("owner denied %s.%s on their own row: %v", p.kind, p.verb, err)
						}
					case w.grants(a, []meta.Owner{globalScope}, p):
						if err != nil {
							t.Fatalf("global binding denied %s.%s on a personal row: %v", p.kind, p.verb, err)
						}
					default:
						if !errors.Is(err, authz.ErrForbidden) {
							t.Fatalf("%s reached %s's personal row for %s.%s: %v",
								a.UserID, owner, p.kind, p.verb, err)
						}
					}
				}
			}
		}
	})
}

// Catalog rows read for everyone and mutate only through a binding. The
// ownerless user-owned row (a catalog row shipped before owners carried an
// id) reads the same way.
func TestProperty_CatalogRowsReadForAllAndMutateOnlyViaBinding(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()
		actors["admin-token"] = &actor.Actor{AdminToken: true}

		ownerless := meta.Owner{Kind: meta.OwnerUser}
		for _, a := range actors {
			ctx := ctxOf(a)
			if err := rbac.Authorize(ctx, "models.get",
				authz.Resource{Kind: "model", Owner: providerOwner()}); err != nil {
				t.Fatalf("catalog read denied: %v", err)
			}
			if err := rbac.Authorize(ctx, "rate-limits.get",
				authz.Resource{Kind: "rate-limit", Owner: &ownerless}); err != nil {
				t.Fatalf("ownerless user-owned catalog row not readable: %v", err)
			}
			if err := rbac.Authorize(ctx, "roles.get",
				authz.Resource{Kind: "role", Owner: &globalScope}); err != nil {
				t.Fatalf("system-owned role not readable: %v", err)
			}

			mutate := rbac.Authorize(ctx, "models.update",
				authz.Resource{Kind: "model", Owner: providerOwner()})
			want := a.AdminToken || w.grants(a, []meta.Owner{globalScope}, probe{"models", "update"})
			if (mutate == nil) != want {
				t.Fatalf("models.update = %v, want allowed = %v (owner chain is [global])", mutate, want)
			}
		}
	})
}

// The break-glass token answers every probe on every owner.
func TestProperty_AdminTokenBypassesEverything(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		ctx := ctxOf(&actor.Actor{AdminToken: true})
		owners := scopedOwners(w, &globalScope, providerOwner(), userOwner(ids.New()))
		for _, p := range probes {
			for _, o := range owners {
				if err := rbac.Authorize(ctx, p.kind+"."+p.verb, resourceOf(p, o)); err != nil {
					t.Fatalf("admin token denied %s.%s: %v", p.kind, p.verb, err)
				}
			}
		}
	})
}

// A disabled project is not in the snapshot, so its chain is [global]: a
// binding at that project grants nothing, and only a global binding does.
func TestProperty_DisabledProjectCollapsesToGlobal(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()

		for _, ps := range w.projects {
			for _, p := range ps {
				if p.Spec.Enabled == nil || *p.Spec.Enabled {
					continue
				}
				owner := projectOwner(p.Meta.ID)
				for _, a := range actors {
					for _, pr := range probes {
						err := rbac.Authorize(ctxOf(a), pr.kind+"."+pr.verb, resourceOf(pr, owner))
						want := w.grants(a, []meta.Owner{globalScope}, pr)
						if (err == nil) != want {
							t.Fatalf("%s.%s on disabled project %s = %v, want allowed = %v",
								pr.kind, pr.verb, p.Meta.ID, err, want)
						}
					}
				}
			}
		}
	})
}

// Invariant 2 at the evaluator: reconciling a team delete leaves exactly
// the decisions a snapshot built without that team would give.
func TestProperty_TeamDeleteMatchesAFreshBuild(t *testing.T) {
	forEachWorld(t, func(t *testing.T, w *world) {
		gone := w.teams[0]

		var keptTeams []*team.Team
		var keptProjects []*project.Project
		for _, tm := range w.teams {
			if tm.Meta.ID == gone.Meta.ID {
				continue
			}
			keptTeams = append(keptTeams, tm)
			keptProjects = append(keptProjects, w.projects[tm.Meta.ID]...)
		}
		fresh := newCatalog(t, keptTeams, projectRows{&keptProjects}, nil, nil, w.roles, w.bindings)

		if err := w.cat.ApplyTeamDelete(gone.Meta.ID); err != nil {
			t.Fatalf("ApplyTeamDelete: %v", err)
		}
		reconciled := w.rbac()
		built := authz.RBAC{Snap: func() authz.Snapshot { return fresh.Current() }}

		owners := scopedOwners(w, &globalScope, providerOwner())
		for _, a := range w.actors() {
			ctx := ctxOf(a)
			for _, p := range probes {
				for _, o := range owners {
					gotR := reconciled.Authorize(ctx, p.kind+"."+p.verb, resourceOf(p, o))
					gotB := built.Authorize(ctx, p.kind+"."+p.verb, resourceOf(p, o))
					if (gotR == nil) != (gotB == nil) {
						t.Fatalf("after deleting %s, %s.%s on %+v: reconcile %v, build %v",
							gone.Meta.Name, p.kind, p.verb, *o, gotR, gotB)
					}
				}
			}
		}
	})
}

// D67: an upgraded deployment has no bindings at all, and every list there
// must answer 200 with the caller's own rows rather than 403.
func TestProperty_ZeroBindingListIsAllowedAndFilteredByVisible(t *testing.T) {
	t.Skip("D67 not implemented: list on a scoped kind with no owner needs a granting binding, so a binding-less actor gets 403 instead of an empty 200")

	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		id := ids.New()
		ctx := ctxOf(actorOf(id, subjectsOf(id)))
		for _, kind := range []string{"keys", "policies", "service-accounts", "projects", "teams"} {
			if err := rbac.Authorize(ctx, kind+".list", authz.Resource{Kind: authz.Singular(kind)}); err != nil {
				t.Errorf("%s.list with no bindings = %v, want allowed", kind, err)
			}
		}
	})
}

// D69: a binding at a project must let its holder resolve the parent team
// row, which today's strict chain refuses.
func TestProperty_ProjectBindingReadsTheParentTeam(t *testing.T) {
	t.Skip("D69 not implemented: a project-scoped binding does not grant teams.get on the parent team")

	forEachWorld(t, func(t *testing.T, w *world) {
		rbac := w.rbac()
		actors := w.actors()
		for _, b := range w.bindings {
			if b.Spec.Scope.Kind != meta.OwnerProject || !w.live(b) {
				continue
			}
			parent := teamScope(w.teamOf(b.Spec.Scope.ID))
			for _, a := range actors {
				if !w.names(a, b) {
					continue
				}
				if err := rbac.Authorize(ctxOf(a), "teams.get",
					authz.Resource{Kind: "team", ID: parent.ID, Owner: &parent}); err != nil {
					t.Errorf("project binding %s cannot read its parent team: %v", b.Meta.Name, err)
				}
			}
		}
	})
}
