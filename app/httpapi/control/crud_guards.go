package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
)

// guardHostKeyPolicyOwnership rejects hostkey create/update when the
// referenced Policy isn't host-owned by the key's HostID. Cross-entity
// invariant the per-row hostkey.Validate() can't enforce (it has no
// access to the policy store). Reads PG directly so disabled rows are
// considered too — a hostkey rebound to a disabled tier policy is still
// a structural mismatch, not just a soft drop. Delete is unaffected.
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
		return refErr(refs(d).HostKey(ctx, incoming))
	}
}

// guardKeyPolicy rejects a key mutation whose Spec.PolicyID
// points at a policy the caller may not see — a key inherits its
// policy's host-keys, so binding to a foreign policy would route traffic
// through someone else's credentials. Reported as "not found" to avoid
// confirming the row exists. Existence of the policy is otherwise still
// not checked here (the inference path handles missing policies).
func guardKeyPolicy(d Deps) mutationGuard[key.Key] {
	return func(ctx context.Context, action string, _, incoming *key.Key) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).PolicyRef(ctx, incoming.Spec.PolicyID, incoming.Meta.Owner))
	}
}

func guardServiceAccount(d Deps) mutationGuard[serviceaccount.ServiceAccount] {
	return func(ctx context.Context, action string, _, incoming *serviceaccount.ServiceAccount) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).ServiceAccount(ctx, incoming))
	}
}

func guardGroupMembers(d Deps) mutationGuard[group.Group] {
	return func(ctx context.Context, action string, _, incoming *group.Group) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).Group(ctx, incoming))
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

func guardPolicyBinding(d Deps) mutationGuard[policybinding.PolicyBinding] {
	return func(ctx context.Context, action string, _, incoming *policybinding.PolicyBinding) error {
		if action == "delete" || incoming == nil {
			return nil
		}
		return refErr(refs(d).PolicyBinding(ctx, incoming))
	}
}
