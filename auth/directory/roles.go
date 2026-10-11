package directory

import (
	"context"

	"github.com/wyolet/relay/auth/principal"
)

func roleMeta(r *Role) *Metadata { return &r.Meta }

// CreateRole adds a role. It grants nothing until a binding names it, so it needs no grant check. An empty owner is the system.
func (d *Directory) CreateRole(ctx context.Context, r *Role) error {
	defaultOwner(&r.Meta)
	if err := prepareCreate(ctx, d.cfg.Stores.Roles, "role", &r.Meta, roleMeta); err != nil {
		return err
	}
	if err := r.validate(d.cfg.Kinds, d.cfg.Verbs); err != nil {
		return err
	}
	return d.writeRole(ctx, r)
}

// UpdateRole replaces the role with id r.Meta.ID. The new rules change what every binding of the role grants, so p must pass the grant check at each scope the role is bound at, as creating those bindings would have required.
func (d *Directory) UpdateRole(ctx context.Context, p *principal.Principal, r *Role) error {
	if _, err := loadForUpdate(ctx, d.cfg.Stores.Roles, "role", &r.Meta, roleMeta); err != nil {
		return err
	}
	defaultOwner(&r.Meta)
	if err := r.validate(d.cfg.Kinds, d.cfg.Verbs); err != nil {
		return err
	}
	bindings, err := d.cfg.Stores.RoleBindings.List(ctx)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		if b.Spec.RoleID != r.Meta.ID {
			continue
		}
		if err := d.checkGrant(ctx, p, r, b.Spec.Scope); err != nil {
			return err
		}
	}
	return d.writeRole(ctx, r)
}

// DeleteRole removes the role; its store takes every binding of it.
func (d *Directory) DeleteRole(ctx context.Context, id string) error {
	return deleteRow(ctx, d, d.cfg.Stores.Roles, KindRole, id)
}

func (d *Directory) writeRole(ctx context.Context, r *Role) error {
	if err := d.cfg.Stores.Roles.Upsert(ctx, r); err != nil {
		return err
	}
	d.notify(ctx, KindRole, OpUpsert, r.Meta.ID)
	return nil
}
