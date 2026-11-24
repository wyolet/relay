package directory

import (
	"context"
	"fmt"
)

func serviceAccountMeta(s *ServiceAccount) *Metadata { return &s.Meta }

// CreateServiceAccount adds a service account to the project or team named by its spec.
func (d *Directory) CreateServiceAccount(ctx context.Context, sa *ServiceAccount) error {
	if err := prepareCreate(ctx, d.cfg.Stores.ServiceAccounts, "service account", &sa.Meta, serviceAccountMeta); err != nil {
		return err
	}
	return d.writeServiceAccount(ctx, sa)
}

// UpdateServiceAccount replaces the service account with id sa.Meta.ID; changing ProjectID or TeamID moves it to another owner.
func (d *Directory) UpdateServiceAccount(ctx context.Context, sa *ServiceAccount) error {
	if _, err := loadForUpdate(ctx, d.cfg.Stores.ServiceAccounts, "service account", &sa.Meta, serviceAccountMeta); err != nil {
		return err
	}
	return d.writeServiceAccount(ctx, sa)
}

// DeleteServiceAccount removes the service account; its store drops it from every binding.
func (d *Directory) DeleteServiceAccount(ctx context.Context, id string) error {
	return deleteRow(ctx, d, d.cfg.Stores.ServiceAccounts, KindServiceAccount, id)
}

func (d *Directory) writeServiceAccount(ctx context.Context, sa *ServiceAccount) error {
	sa.StampOwner()
	if err := sa.validate(); err != nil {
		return err
	}
	var found bool
	switch o := sa.Meta.Owner; o.Kind {
	case OwnerProject:
		p, err := d.cfg.Stores.Projects.Get(ctx, o.ID)
		if err != nil {
			return err
		}
		found = p != nil
	case OwnerTeam:
		t, err := d.cfg.Stores.Teams.Get(ctx, o.ID)
		if err != nil {
			return err
		}
		found = t != nil
	}
	if !found {
		return fmt.Errorf("%w: service account %q: %s %q does not exist", ErrInvalid, sa.Meta.Name, sa.Meta.Owner.Kind, sa.Meta.Owner.ID)
	}
	if err := d.cfg.Stores.ServiceAccounts.Upsert(ctx, sa); err != nil {
		return err
	}
	d.notify(ctx, KindServiceAccount, OpUpsert, sa.Meta.ID)
	return nil
}
