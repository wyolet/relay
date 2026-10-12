package authz_test

import (
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/rbac"
	"github.com/wyolet/relay/pkg/ids"
)

func TestExplainReasons(t *testing.T) {
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	roleID := map[string]string{}
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		roleID[r.Meta.Name] = r.Meta.ID
	}
	teamID, p1, p2 := ids.New(), ids.New(), ids.New()
	uProj, uTeam, uGroup, stranger := ids.New(), ids.New(), ids.New(), ids.New()
	eng := &group.Group{}
	eng.Meta = meta.Metadata{ID: ids.New(), Name: "eng", Owner: globalScope}
	eng.Spec.MemberIDs = []string{uGroup}
	eng.Spec.Enabled = &yes
	cat := catalogtest.Catalog{
		Teams:    []*team.Team{mkTeam(teamID, "t")},
		Projects: []*project.Project{mkProject(p1, "p1", teamID, true), mkProject(p2, "p2", teamID, true)},
		Groups:   []*group.Group{eng},
		Roles:    builtins,
		RoleBindings: []*rolebinding.RoleBinding{
			mkBinding("p1-dev", roleID["developer"], projectScope(p1), userSubject(uProj)),
			mkBinding("t-admin", roleID["team-admin"], teamScope(teamID), userSubject(uTeam)),
			mkBinding("eng-view", roleID["viewer"], projectScope(p2), groupSubject("eng")),
		},
	}.Load(t)
	snap := cat.Current()
	r := authz.RBAC{Snap: func() authz.Snapshot { return snap }}
	keyIn := func(p string) authz.Resource {
		return authz.Resource{Kind: "key", ID: ids.New(), Owner: projectOwner(p)}
	}
	chainOf := func(p string) []rbac.Scope {
		return []rbac.Scope{{Kind: "project", ID: p}, {Kind: "team", ID: teamID}, {Kind: "system"}}
	}

	tests := []struct {
		name    string
		a       *actor.Actor
		action  string
		res     authz.Resource
		reason  rbac.Reason
		allowed bool
		rule    string
		binding string
		chain   []rbac.Scope
	}{
		{name: "admin role", a: actorOf(stranger, subjectsOf(stranger), user.RoleAdmin), action: "keys.delete", res: keyIn(p1),
			reason: rbac.ReasonAdmin, allowed: true},
		{name: "admin token", a: &actor.Actor{AdminToken: true}, action: "settings.update", res: authz.Resource{Kind: "settings"},
			reason: rbac.ReasonAdmin, allowed: true},
		{name: "personal row", a: actorOf(stranger, subjectsOf(stranger)), action: "keys.delete",
			res: authz.Resource{Kind: "key", ID: ids.New(), Owner: userOwner(stranger)}, reason: rbac.ReasonRule, allowed: true, rule: "personal-row"},
		{name: "catalog read", a: actorOf(stranger, subjectsOf(stranger)), action: "models.get",
			res: authz.Resource{Kind: "model", ID: ids.New(), Owner: providerOwner()}, reason: rbac.ReasonRule, allowed: true, rule: "catalog-read"},
		{name: "scoped list", a: actorOf(stranger, subjectsOf(stranger)), action: "keys.list", res: authz.Resource{Kind: "key"},
			reason: rbac.ReasonRule, allowed: true, rule: "scoped-list"},
		{name: "direct binding", a: actorOf(uProj, subjectsOf(uProj)), action: "keys.create", res: authz.Resource{Kind: "key", Owner: projectOwner(p1)},
			reason: rbac.ReasonBinding, allowed: true, binding: "p1-dev", chain: chainOf(p1)},
		{name: "group binding", a: actorOf(uGroup, subjectsOf(uGroup, "eng")), action: "keys.get", res: keyIn(p2),
			reason: rbac.ReasonBinding, allowed: true, binding: "eng-view", chain: chainOf(p2)},
		{name: "team binding reaches a project", a: actorOf(uTeam, subjectsOf(uTeam)), action: "keys.delete", res: keyIn(p2),
			reason: rbac.ReasonBinding, allowed: true, binding: "t-admin", chain: chainOf(p2)},
		{name: "sibling project", a: actorOf(uProj, subjectsOf(uProj)), action: "keys.get", res: keyIn(p2),
			reason: rbac.ReasonNone, chain: chainOf(p2)},
		{name: "role lacks the verb", a: actorOf(uGroup, subjectsOf(uGroup, "eng")), action: "keys.delete", res: keyIn(p2),
			reason: rbac.ReasonNone, chain: chainOf(p2)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			x, err := r.Explain(t.Context(), tt.a, tt.action, tt.res)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			if x.Allowed != tt.allowed || x.Reason != tt.reason || x.Rule != tt.rule {
				t.Fatalf("Explain = %+v, want allowed=%v reason=%s rule=%q", x, tt.allowed, tt.reason, tt.rule)
			}
			if authorized := r.Authorize(ctxOf(tt.a), tt.action, tt.res) == nil; authorized != x.Allowed {
				t.Fatalf("Authorize allowed = %v, Explain allowed = %v", authorized, x.Allowed)
			}
			b, found := authz.MatchedBinding(snap, x)
			if found != (tt.binding != "") || (found && b.Meta.Name != tt.binding) {
				t.Fatalf("matched binding = %v (%v), want %q", b, found, tt.binding)
			}
			if len(x.Chain) != len(tt.chain) {
				t.Fatalf("chain = %+v, want %+v", x.Chain, tt.chain)
			}
			for i := range tt.chain {
				if x.Chain[i] != tt.chain[i] {
					t.Fatalf("chain = %+v, want %+v", x.Chain, tt.chain)
				}
			}
		})
	}
}

func TestExplainRefusesUnauthenticated(t *testing.T) {
	r := authz.RBAC{}
	for _, a := range []*actor.Actor{nil, {}} {
		if x, err := r.Explain(t.Context(), a, "keys.get", authz.Resource{Kind: "key"}); !errors.Is(err, authz.ErrUnauthenticated) || x.Allowed {
			t.Fatalf("Explain(%+v) = %+v, %v; want unauthenticated", a, x, err)
		}
	}
}

// The audit wrapper explains through the authorizer it wraps, and says so when that one cannot.
func TestAuditAuthorizerExplains(t *testing.T) {
	a := &actor.Actor{AdminToken: true}
	x, err := audit.Authorizer{Inner: authz.RBAC{}}.Explain(t.Context(), a, "keys.get", authz.Resource{Kind: "key"})
	if err != nil || !x.Allowed || x.Reason != rbac.ReasonAdmin {
		t.Fatalf("wrapped RBAC = %+v, %v", x, err)
	}
	if _, err := (audit.Authorizer{Inner: authz.AlwaysAllowAuthenticated{}}).Explain(t.Context(), a, "keys.get", authz.Resource{Kind: "key"}); !errors.Is(err, authz.ErrNoExplanation) {
		t.Fatalf("wrapped single-user authorizer = %v, want ErrNoExplanation", err)
	}
}
