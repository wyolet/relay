package authz_test

import (
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

// A global binding of the built-in admin role is an admin for the organization's rows, as the admin role and token are; the same role bound in a team, or a custom role of that name, is not.
func TestGlobalAdminBinding(t *testing.T) {
	builtins, err := role.Builtins()
	if err != nil {
		t.Fatalf("built-in roles: %v", err)
	}
	var adminRoleID string
	for _, r := range builtins {
		r.Meta.ID, r.Spec.Enabled = ids.New(), &yes
		if r.Meta.Name == role.BuiltinAdmin {
			adminRoleID = r.Meta.ID
		}
	}
	custom := &role.Role{Meta: meta.Metadata{ID: ids.New(), Name: role.BuiltinAdmin, Owner: teamScope(t1ID)}}
	custom.Spec = builtins[0].Spec
	globalAdmin, teamBound, customBound := ids.New(), ids.New(), ids.New()
	cat := catalogtest.Catalog{
		Roles: append(builtins, custom),
		RoleBindings: []*rolebinding.RoleBinding{
			mkBinding("global-admin", adminRoleID, globalScope, userSubject(globalAdmin)),
			mkBinding("team-admin-role", adminRoleID, teamScope(t1ID), userSubject(teamBound)),
			mkBinding("custom-admin", custom.Meta.ID, globalScope, userSubject(customBound)),
		},
	}.Load(t)
	r := authz.RBAC{Snap: func() authz.Snapshot { return cat.Current() }}
	org := &meta.Owner{Kind: meta.OwnerOrganization}
	for name, tc := range map[string]struct {
		id   string
		want bool
	}{
		"global admin binding":    {globalAdmin, true},
		"admin role in a team":    {teamBound, false},
		"custom role named admin": {customBound, false},
	} {
		ctx := ctxOf(actorOf(tc.id, subjectsOf(tc.id)))
		if got := authz.Admin(ctx, r); got != tc.want {
			t.Errorf("%s: Admin = %v, want %v", name, got, tc.want)
		}
		err := r.Authorize(ctx, "policies.update", authz.Resource{Kind: "policy", ID: ids.New(), Owner: org})
		if (err == nil) != tc.want {
			t.Errorf("%s: update of an organization policy = %v, want allowed %v", name, err, tc.want)
		}
	}
	if authz.Admin(ctxOf(actorOf(globalAdmin, subjectsOf(globalAdmin))), authz.AlwaysAllowAuthenticated{}) {
		t.Error("Admin read a role binding without an RBAC authorizer")
	}
}
