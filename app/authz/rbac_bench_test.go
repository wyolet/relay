package authz_test

import (
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/ids"
)

// benchRBAC is a team with two projects and a developer bound at the first, who also belongs to a group bound at the first.
func benchRBAC(b *testing.B) (authz.RBAC, string, string, string) {
	b.Helper()
	builtins, err := role.Builtins()
	if err != nil {
		b.Fatalf("built-in roles: %v", err)
	}
	roleID := map[string]string{}
	for _, r := range builtins {
		r.Meta.ID = ids.New()
		r.Spec.Enabled = &yes
		roleID[r.Meta.Name] = r.Meta.ID
	}
	teamID, own, sibling, userID := ids.New(), ids.New(), ids.New(), ids.New()
	g := &group.Group{}
	g.Meta = meta.Metadata{ID: ids.New(), Name: "devs", Owner: globalScope}
	g.Spec.MemberIDs = []string{userID}
	g.Spec.Enabled = &yes
	cat := catalogtest.Catalog{
		Teams:    []*team.Team{mkTeam(teamID, "t")},
		Projects: []*project.Project{mkProject(own, "own", teamID, true), mkProject(sibling, "sibling", teamID, true)},
		Groups:   []*group.Group{g},
		Roles:    builtins,
		RoleBindings: []*rolebinding.RoleBinding{
			mkBinding("dev", roleID["developer"], projectScope(own), userSubject(userID)),
			mkBinding("viewer", roleID["viewer"], projectScope(own), groupSubject("devs")),
		},
	}.Load(b)
	return authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}, userID, own, sibling
}

func BenchmarkRBACAuthorizeProjectGet(b *testing.B) {
	rbac, userID, own, _ := benchRBAC(b)
	ctx := ctxOf(actorOf(userID, subjectsOf(userID, "devs")))
	res := authz.Resource{Kind: "key", ID: ids.New(), Owner: projectOwner(own)}
	b.ReportAllocs()
	for b.Loop() {
		if err := rbac.Authorize(ctx, "keys.get", res); err != nil {
			b.Fatal(err)
		}
	}
}

// A sibling project's row is refused only after every binding is scanned.
func BenchmarkRBACVisibleSiblingDenied(b *testing.B) {
	rbac, userID, _, sibling := benchRBAC(b)
	ctx := ctxOf(actorOf(userID, subjectsOf(userID, "devs")))
	owner := *projectOwner(sibling)
	b.ReportAllocs()
	for b.Loop() {
		if rbac.Visible(ctx, "key", "", owner) {
			b.Fatal("sibling row visible")
		}
	}
}
