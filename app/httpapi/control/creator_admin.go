package control

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/pkg/meta"
)

// grantCreatorAdmin binds the signed-in user who created a team or project to that scope's built-in admin role, so they can run what they made and the plans open their member account there. The admin token is no user and gets none; a binding already granting the role is left alone. Apply does not come here: a manifest states its bindings.
func grantCreatorAdmin(ctx context.Context, d Deps, ops directoryOps, scope meta.Owner, row meta.Metadata, roleName string) error {
	a := actor.From(ctx)
	if a == nil || a.AdminToken || a.UserID == "" || d.Users == nil || d.Stores == nil || d.Stores.Role == nil || d.Stores.RoleBinding == nil {
		return nil
	}
	if u, err := d.Users.Get(ctx, a.UserID); err != nil || u == nil {
		return err
	}
	roles, err := d.Stores.Role.List(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(roles, func(r *role.Role) bool { return r.Meta.Name == roleName && r.Meta.Owner.Kind == meta.OwnerSystem })
	if i < 0 {
		slog.Warn("control: built-in role missing; the creator gets no admin binding", "role", roleName, "scope", scope.Kind, "id", scope.ID)
		return nil
	}
	roleID := roles[i].Meta.ID
	bindings, err := d.Stores.RoleBinding.List(ctx)
	if err != nil {
		return err
	}
	creator := rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: a.UserID}
	for _, b := range bindings {
		if b.Spec.RoleID == roleID && b.Spec.Scope == scope && slices.Contains(b.Spec.Subjects, creator) {
			return nil
		}
	}
	dir, err := ops.directory()
	if err != nil {
		return err
	}
	rb := &rolebinding.RoleBinding{
		Meta: meta.Metadata{
			DisplayName: label(row) + " admins",
			Description: fmt.Sprintf("Made when %s created the %s.", a.Username, scope.Kind),
		},
		Spec: rolebinding.Spec{RoleID: roleID, Scope: scope, Subjects: []rolebinding.Subject{creator}},
	}
	rb.StampOwner()
	// The binding is part of the create the caller was authorized for, so it takes no grant check of its own.
	if err := dir.CreateRoleBinding(ctx, &principal.Principal{ID: a.UserID, Admin: true}, rb.ToDirectory()); err != nil {
		return fmt.Errorf("%s %q was created, but its admin role binding was not: %w", scope.Kind, row.Name, meta.FromDirectoryError(err))
	}
	return nil
}

// label is a row's display name, or its name when it has none.
func label(m meta.Metadata) string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Name
}
