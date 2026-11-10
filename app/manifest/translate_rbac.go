package manifest

import (
	"fmt"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/meta"
)

// ---------------------------------------------------------------------------
// Role / RoleBinding / PolicyBinding
// ---------------------------------------------------------------------------

// ToRole translates through the auth module, which makes every role system-owned: a role is a rule set anyone may be bound to, so ownership is relay's, not the manifest author's.
func ToRole(d RoleDTO, idx Resolver) (*role.Role, error) {
	row, err := AuthManifests.ToRole(roleDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return role.FromDirectory(row), nil
}

func FromRole(r *role.Role, rev ReverseResolver) RoleDTO {
	d, _ := fromAuthDoc(rendered(AuthManifests.FromRole(r.ToDirectory(), authNames{rev})))
	dto := *d.Role
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(r.Meta)
	return dto
}

// ToRoleBinding resolves the role name, the scope target, and the subjects
// through the auth module. Owner mirrors spec.scope, so it is re-derived
// rather than read from the wire form.
func ToRoleBinding(d RoleBindingDTO, idx Resolver) (*rolebinding.RoleBinding, error) {
	row, err := AuthManifests.ToRoleBinding(roleBindingDoc(d), authRefs{idx})
	if err != nil {
		return nil, fromAuthError(err)
	}
	return rolebinding.FromDirectory(row), nil
}

func FromRoleBinding(b *rolebinding.RoleBinding, rev ReverseResolver) RoleBindingDTO {
	d, _ := fromAuthDoc(rendered(AuthManifests.FromRoleBinding(b.ToDirectory(), authNames{rev})))
	dto := *d.RoleBinding
	dto.APIVersion, dto.Metadata = APIVersion, metaToWire(b.Meta)
	dto.Metadata.Owner.Name = dto.Spec.Scope.Name
	return dto
}

// ToPolicyBinding resolves the owner, the policy and the accounts by name.
func ToPolicyBinding(d PolicyBindingDTO, idx Resolver) (*policybinding.PolicyBinding, error) {
	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	policyID, ok := idx.PolicyID(d.Spec.Policy)
	if !ok {
		return nil, refNotFound("policybinding %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
	}
	if len(d.Spec.Accounts) == 0 {
		return nil, fmt.Errorf("policybinding %q: spec.accounts names no account", d.Metadata.Name)
	}
	accounts, err := resolveNames(idx.AccountID, d.Spec.Accounts, "policybinding %q: account %q not found", d.Metadata.Name)
	if err != nil {
		return nil, err
	}
	return &policybinding.PolicyBinding{
		Meta: m,
		Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: accounts, Enabled: d.Spec.Enabled},
	}, nil
}

func FromPolicyBinding(b *policybinding.PolicyBinding, rev ReverseResolver) PolicyBindingDTO {
	wm := metaToWire(b.Meta)
	switch o := b.Meta.Owner; o.Kind {
	case meta.OwnerProject:
		wm.Owner.Name = nameOr(rev.ProjectName, o.ID)
	case meta.OwnerTeam:
		wm.Owner.Name = nameOr(rev.TeamName, o.ID)
	}
	return PolicyBindingDTO{
		APIVersion: APIVersion,
		Kind:       "PolicyBinding",
		Metadata:   wm,
		Spec: PolicyBindingSpec{
			Policy:   nameOr(rev.PolicyName, b.Spec.PolicyID),
			Accounts: namesOf(rev.AccountName, b.Spec.AccountIDs),
			Enabled:  b.Spec.Enabled,
		},
	}
}
