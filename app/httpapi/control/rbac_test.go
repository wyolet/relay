package control

import (
	"context"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

func customRole(name string) *role.Role {
	return &role.Role{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: role.Spec{Rules: []role.Rule{{Kinds: []string{"keys"}, Verbs: []string{"get"}}}},
	}
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	se, ok := err.(huma.StatusError)
	if !ok {
		t.Fatalf("error %v is not a huma.StatusError", err)
	}
	return se.GetStatus()
}

func TestGuardRole(t *testing.T) {
	licensed := &fakeLicense{info: license.Info{Licensed: true, Features: []string{license.FeatureCustomRoles}}}
	community := &fakeLicense{}

	for _, tc := range []struct {
		name   string
		lic    license.Service
		action string
		role   *role.Role
		want   int // 0 = allowed
	}{
		{name: "custom role under a license", lic: licensed, action: "create", role: customRole("release-manager")},
		{name: "built-in name reserved", lic: licensed, action: "create", role: customRole("admin"), want: 400},
		{name: "built-in name reserved before the license check", lic: community, action: "create", role: customRole("developer"), want: 400},
		{name: "custom role without a license", lic: community, action: "create", role: customRole("release-manager"), want: 403},
		{name: "update without a license", lic: community, action: "update", role: customRole("release-manager"), want: 403},
		{name: "no license service at all", lic: nil, action: "create", role: customRole("release-manager"), want: 403},
		{name: "delete stays open", lic: community, action: "delete", role: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardRole(Deps{License: tc.lic})
			err := guard(context.Background(), tc.action, nil, tc.role)
			if got := statusOf(t, err); got != tc.want {
				t.Fatalf("guard = %v (status %d), want status %d", err, got, tc.want)
			}
		})
	}
}

func TestGuardRoleBindingStampsOwner(t *testing.T) {
	teamID := meta.NewID()
	b := &rolebinding.RoleBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "platform-admins"},
		Spec: rolebinding.Spec{
			RoleID:   meta.NewID(),
			Scope:    meta.Owner{Kind: meta.OwnerTeam, ID: teamID},
			Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "platform-eng"}},
		},
	}
	if err := guardRoleBinding(Deps{})(context.Background(), "create", nil, b); err != nil {
		t.Fatalf("guard: %v", err)
	}
	if b.Meta.Owner != (meta.Owner{Kind: meta.OwnerTeam, ID: teamID}) {
		t.Fatalf("owner = %+v, want the team scope", b.Meta.Owner)
	}
	if err := b.Validate(); err != nil {
		t.Fatalf("stamped binding does not validate: %v", err)
	}
}

func TestGuardPolicyBindingDefaultsPriority(t *testing.T) {
	projectID := meta.NewID()
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search-everyone"},
		Spec: policybinding.Spec{
			ProjectID: projectID,
			PolicyID:  meta.NewID(),
			Subjects:  []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}},
		},
	}
	if err := guardPolicyBinding(Deps{})(context.Background(), "create", nil, b); err != nil {
		t.Fatalf("guard: %v", err)
	}
	if b.Spec.Priority == nil || *b.Spec.Priority != policybinding.DefaultPriority {
		t.Errorf("priority = %v, want %d", b.Spec.Priority, policybinding.DefaultPriority)
	}
	if b.Meta.Owner != (meta.Owner{Kind: meta.OwnerProject, ID: projectID}) {
		t.Errorf("owner = %+v, want the project", b.Meta.Owner)
	}
}
