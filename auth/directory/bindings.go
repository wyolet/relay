package directory

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/auth/principal"
)

func bindingMeta(b *RoleBinding) *Metadata { return &b.Meta }

// CreateRoleBinding grants a role to the binding's subjects at its scope. Binding hands out every permission in the role, so p must already hold each one there.
func (d *Directory) CreateRoleBinding(ctx context.Context, p *principal.Principal, b *RoleBinding) error {
	if err := prepareCreate(ctx, d.cfg.Stores.RoleBindings, "role binding", &b.Meta, bindingMeta); err != nil {
		return err
	}
	return d.writeBinding(ctx, p, b)
}

// UpdateRoleBinding replaces the binding with id b.Meta.ID, with the grant check of a create.
func (d *Directory) UpdateRoleBinding(ctx context.Context, p *principal.Principal, b *RoleBinding) error {
	if _, err := loadForUpdate(ctx, d.cfg.Stores.RoleBindings, "role binding", &b.Meta, bindingMeta); err != nil {
		return err
	}
	return d.writeBinding(ctx, p, b)
}

// DeleteRoleBinding removes the binding. Taking a grant away needs no grant check.
func (d *Directory) DeleteRoleBinding(ctx context.Context, id string) error {
	return deleteRow(ctx, d, d.cfg.Stores.RoleBindings, KindRoleBinding, id)
}

func (d *Directory) writeBinding(ctx context.Context, p *principal.Principal, b *RoleBinding) error {
	b.StampOwner()
	if err := b.validate(); err != nil {
		return err
	}
	s := d.cfg.Stores
	r, err := s.Roles.Get(ctx, b.Spec.RoleID)
	if err != nil {
		return err
	}
	if r == nil {
		return fmt.Errorf("%w: role binding %q: role %q does not exist", ErrInvalid, b.Meta.Name, b.Spec.RoleID)
	}
	if err := d.checkGrant(ctx, p, r, b.Spec.Scope); err != nil {
		return err
	}
	if err := d.checkScopeExists(ctx, b); err != nil {
		return err
	}
	if err := d.checkSubjectsExist(ctx, b); err != nil {
		return err
	}
	if err := s.RoleBindings.Upsert(ctx, b); err != nil {
		return err
	}
	d.notify(ctx, KindRoleBinding, OpUpsert, b.Meta.ID)
	return nil
}

// checkScopeExists refuses a team or project scope with no row behind it; any other scope kind is the product's and is not looked up.
func (d *Directory) checkScopeExists(ctx context.Context, b *RoleBinding) error {
	var found bool
	switch b.Spec.Scope.Kind {
	case OwnerTeam:
		t, err := d.cfg.Stores.Teams.Get(ctx, b.Spec.Scope.ID)
		if err != nil {
			return err
		}
		found = t != nil
	case OwnerProject:
		p, err := d.cfg.Stores.Projects.Get(ctx, b.Spec.Scope.ID)
		if err != nil {
			return err
		}
		found = p != nil
	default:
		return nil
	}
	if !found {
		return fmt.Errorf("%w: role binding %q: %s %q does not exist", ErrInvalid, b.Meta.Name, b.Spec.Scope.Kind, b.Spec.Scope.ID)
	}
	return nil
}

// checkSubjectsExist refuses a user or service-account subject with no row. Group subjects are names and are not looked up.
func (d *Directory) checkSubjectsExist(ctx context.Context, b *RoleBinding) error {
	for _, sub := range b.Spec.Subjects {
		var found bool
		switch sub.Kind {
		case SubjectUser:
			u, err := d.cfg.Stores.Users.Get(ctx, sub.ID)
			if err != nil {
				return err
			}
			found = u != nil
		case SubjectServiceAccount:
			sa, err := d.cfg.Stores.ServiceAccounts.Get(ctx, sub.ID)
			if err != nil {
				return err
			}
			found = sa != nil
		default:
			continue
		}
		if !found {
			return fmt.Errorf("%w: role binding %q: subject %q does not exist", ErrInvalid, b.Meta.Name, sub.Key())
		}
	}
	return nil
}
