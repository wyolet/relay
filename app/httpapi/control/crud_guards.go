package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
)

// guardHostKey checks the key's host and account, which hostkey.Validate
// cannot reach, and refuses an owner move that would strand the policies
// linking the key. Stored values rotate through their own endpoint.
func guardHostKey(d Deps) mutationGuard[hostkey.HostKey] {
	return func(ctx context.Context, action string, existing, incoming *hostkey.HostKey) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		rotating := incoming.Spec.Value != "" && (existing == nil || incoming.Spec.Value != existing.Resolved)
		if action == "update" && rotating &&
			(incoming.Spec.ValueFrom.Kind == hostkey.ValueKindStored || incoming.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth) {
			return fmt.Errorf("value cannot be set on update — use POST /host-keys/by-id/{id}/rotate to rotate the credential")
		}
		if err := refs(d).HostKey(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action == "update" && existing.Meta.Owner != incoming.Meta.Owner {
			return refuseHostKeyMove(ctx, d, incoming)
		}
		return nil
	}
}

func guardServiceAccount(d Deps) mutationGuard[serviceaccount.ServiceAccount] {
	return func(ctx context.Context, action string, existing, incoming *serviceaccount.ServiceAccount) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		if action == "update" && existing.Spec.ProjectID != incoming.Spec.ProjectID {
			return refuseServiceAccountMove(existing)
		}
		if err := refs(d).ServiceAccount(ctx, incoming); err != nil {
			return refErr(err)
		}
		return nil
	}
}

func guardGroup(d Deps) mutationGuard[group.Group] {
	return func(ctx context.Context, action string, existing, incoming *group.Group) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		c := refs(d)
		if err := c.Group(ctx, incoming); err != nil {
			return refErr(err)
		}
		if action == "update" && existing != nil {
			return refErr(c.GroupRename(ctx, existing.Meta.Name, incoming.Meta.Name))
		}
		return nil
	}
}

func guardProject(d Deps) mutationGuard[project.Project] {
	return func(ctx context.Context, action string, _, incoming *project.Project) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).Project(ctx, incoming))
	}
}

// guardRole reserves the built-in role names for the seeded system rows and
// gates authoring a custom role on the license. Delete stays open so an
// expired license never traps a row an operator wants gone.
func guardRole(d Deps) mutationGuard[role.Role] {
	return func(ctx context.Context, action string, existing, incoming *role.Role) error {
		// Built-ins are the relay's own rows: edits go through the seed. An
		// admin may delete one nothing references, since boot seeds it again.
		if existing != nil && role.IsBuiltin(existing.Meta.Name) && (action != "delete" || !authz.IsAdmin(ctx)) {
			return huma.Error403Forbidden(fmt.Sprintf("role %q is built in: %s goes through the seed, not generic CRUD", existing.Meta.Name, action))
		}
		if action == "delete" || incoming == nil {
			return nil
		}
		if role.IsBuiltin(incoming.Meta.Name) {
			return huma.Error400BadRequest(fmt.Sprintf("role name %q is reserved for the built-in role", incoming.Meta.Name))
		}
		if d.License == nil || !d.License.Has(license.FeatureCustomRoles) {
			return huma.Error403Forbidden(license.ErrRequired.Error())
		}
		return refErr(refs(d).Role(ctx, incoming))
	}
}

func guardRoleBinding(d Deps) mutationGuard[rolebinding.RoleBinding] {
	return func(ctx context.Context, action string, _, incoming *rolebinding.RoleBinding) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).RoleBinding(ctx, incoming))
	}
}

// policyBindingChecks are the checks the policy-bindings routes run, shared with PUT /accounts/{ref}/policy.
func policyBindingChecks(d Deps) kindChecks[policybinding.PolicyBinding] {
	return kindChecks[policybinding.PolicyBinding]{
		plural: "policy-bindings", singular: "policy-binding", authz: d.Authz, gov: d.Catalog,
		validate: func(b *policybinding.PolicyBinding) error { return b.Validate() },
		guard:    guardPolicyBinding(d),
	}
}

func guardPolicyBinding(d Deps) mutationGuard[policybinding.PolicyBinding] {
	return func(ctx context.Context, action string, _, incoming *policybinding.PolicyBinding) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).PolicyBinding(ctx, incoming))
	}
}
