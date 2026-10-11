package control

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/directory/memstore"
	"github.com/wyolet/relay/auth/rbac"
)

// probeAuthz records every action it is asked and refuses one of them.
type probeAuthz struct {
	asked  []string
	refuse string
}

func (p *probeAuthz) Authorize(_ context.Context, action string, res authz.Resource) error {
	p.asked = append(p.asked, action+" "+res.Kind+" "+string(res.Owner.Kind)+":"+res.Owner.ID)
	if action == p.refuse {
		return authz.ErrForbidden
	}
	return nil
}

func (p *probeAuthz) Visible(context.Context, string, string, meta.Owner) bool { return true }

// The directory's grant check is relay's: every permission the role holds, probed at the binding's scope, under relay's singular kind names. Its probes skip the audit wrapper, which marks a request by its last mutating decision.
func TestDirectoryGrantCheckProbesTheUnauditedAuthorizer(t *testing.T) {
	inner := &probeAuthz{refuse: "keys.delete"}
	d := Deps{Authz: audit.Authorizer{Inner: inner}, Stores: &appcatalog.Stores{Directory: memstore.New().Stores()}}
	if unaudited(d.Authz) != authz.Authorizer(inner) {
		t.Fatal("the grant check would probe through the audit wrapper")
	}
	ops := newDirectoryOps(d)
	dir, err := ops.directory()
	if err != nil {
		t.Fatal(err)
	}
	ctx := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	team := &directory.Team{Meta: directory.Metadata{Name: "platform"}}
	r := &directory.Role{Meta: directory.Metadata{Name: "key-admin"},
		Spec: directory.RoleSpec{Rules: []rbac.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get", "delete"}}}}}
	if err := dir.CreateTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	if err := dir.CreateRole(ctx, r); err != nil {
		t.Fatal(err)
	}
	b := &directory.RoleBinding{Meta: directory.Metadata{Name: "key-admins"}, Spec: directory.RoleBindingSpec{
		RoleID: r.Meta.ID, Scope: rbac.Scope{Kind: directory.OwnerTeam, ID: team.Meta.ID},
		Subjects: []directory.Subject{{Kind: directory.SubjectGroup, Name: "eng"}}}}

	err = dir.CreateRoleBinding(ctx, principalOf(ctx, d.Authz), b)
	if !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("binding a role with a permission the caller lacks = %v, want forbidden", err)
	}
	want := []string{"keys.get key team:" + team.Meta.ID, "keys.delete key team:" + team.Meta.ID}
	if len(inner.asked) != len(want) || inner.asked[0] != want[0] || inner.asked[1] != want[1] {
		t.Fatalf("probes = %q, want %q", inner.asked, want)
	}

	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-0", Roles: []string{user.RoleAdmin}})
	inner.asked = nil
	if err := dir.CreateRoleBinding(admin, principalOf(admin, d.Authz), b); err != nil {
		t.Fatalf("admin binding: %v", err)
	}
	if len(inner.asked) != 0 {
		t.Fatalf("an admin's grant was probed: %q", inner.asked)
	}
}

// The directory's admin is relay's RequireAdmin: the admin role or token under a scoping authorizer, anyone without one.
func TestDirectoryPrincipalIsAdminWhereRelayIs(t *testing.T) {
	member := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	admin := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-0", Roles: []string{user.RoleAdmin}})
	token := actor.WithActor(context.Background(), &actor.Actor{AdminToken: true})
	for _, tt := range []struct {
		name string
		ctx  context.Context
		az   authz.Authorizer
		want bool
	}{
		{"member, scoped", member, scopedAllow{}, false},
		{"admin role, scoped", admin, scopedAllow{}, true},
		{"admin token, scoped", token, scopedAllow{}, true},
		{"member, single-user", member, allowAuthz{}, true},
	} {
		if p := principalOf(tt.ctx, tt.az); p.Admin != tt.want {
			t.Errorf("%s: Admin = %v, want %v", tt.name, p.Admin, tt.want)
		}
	}
	if p := principalOf(member, scopedAllow{}); p.ID != "u-1" {
		t.Errorf("principal id = %q, want the actor's", p.ID)
	}
}
