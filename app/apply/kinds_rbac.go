package apply

import (
	"context"

	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
)

func (b *builder) planGroupsAndRoles(ctx context.Context, grpDocs []*manifest.GroupDTO, roleDocs []*manifest.RoleDTO) error {
	s := b.opts.Stores
	if err := planKind(ctx, b, kindWiring[manifest.GroupDTO, group.Group]{
		Kind: "Group", Docs: grpDocs, Names: b.idx.Groups, Rows: b.rows.Groups,
		To: manifest.ToGroup, Meta: func(g *group.Group) *meta.Metadata { return &g.Meta },
		Upsert: s.Group.Upsert, Delete: s.Group.Delete,
		Check: refsFor(b, refcheck.Checker.Group),
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.RoleDTO, role.Role]{
		Kind: "Role", Docs: roleDocs, Names: b.idx.Roles, Rows: b.rows.Roles,
		To: manifest.ToRole, Meta: func(r *role.Role) *meta.Metadata { return &r.Meta },
		Upsert: s.Role.Upsert, Delete: s.Role.Delete,
		Check: refsFor(b, refcheck.Checker.Role),
	}); err != nil {
		return err
	}
	return nil
}

func (b *builder) planBindings(ctx context.Context, rbDocs []*manifest.RoleBindingDTO, pbDocs []*manifest.PolicyBindingDTO) error {
	s := b.opts.Stores
	if err := planKind(ctx, b, kindWiring[manifest.RoleBindingDTO, rolebinding.RoleBinding]{
		Kind: "RoleBinding", Docs: rbDocs, Names: b.idx.RoleBindings, Rows: b.rows.RoleBindings,
		To: manifest.ToRoleBinding, Meta: func(x *rolebinding.RoleBinding) *meta.Metadata { return &x.Meta },
		Upsert: s.RoleBinding.Upsert, Delete: s.RoleBinding.Delete,
		Check: refsFor(b, refcheck.Checker.RoleBinding),
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.PolicyBindingDTO, policybinding.PolicyBinding]{
		Kind: "PolicyBinding", Docs: pbDocs, Names: b.idx.PolicyBindings, Rows: b.rows.PolicyBindings,
		To: manifest.ToPolicyBinding, Meta: func(x *policybinding.PolicyBinding) *meta.Metadata { return &x.Meta },
		Upsert: s.PolicyBinding.Upsert, Delete: s.PolicyBinding.Delete,
		Check: refsFor(b, refcheck.Checker.PolicyBinding),
	}); err != nil {
		return err
	}
	return nil
}

// checkRoleDocs refuses Role documents apply must not write: a name the
// built-ins own (which would shadow a system role every binding trusts), and
// a custom role on a deployment that is not licensed for one.
func (b *builder) checkRoleDocs(docs []*manifest.RoleDTO) error {
	for _, d := range docs {
		if role.IsBuiltin(d.Metadata.Name) {
			return &ReservedNameError{Kind: "Role", Name: d.Metadata.Name}
		}
		if b.lic != nil && !b.lic.Has(license.FeatureCustomRoles) {
			return &LicenseError{Kind: "Role", Name: d.Metadata.Name, Feature: license.FeatureCustomRoles}
		}
	}
	return nil
}
