package directory

import (
	"context"
	"fmt"
)

func serviceAccountMeta(s *ServiceAccount) *Metadata { return &s.Meta }

// CreateServiceAccount adds a service account to the project named by its spec.
func (d *Directory) CreateServiceAccount(ctx context.Context, sa *ServiceAccount) error {
	if err := prepareCreate(ctx, d.cfg.Stores.ServiceAccounts, "service account", &sa.Meta, serviceAccountMeta); err != nil {
		return err
	}
	return d.writeServiceAccount(ctx, sa)
}

// UpdateServiceAccount replaces the service account with id sa.Meta.ID; changing ProjectID moves it to another project.
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
	p, err := d.cfg.Stores.Projects.Get(ctx, sa.Spec.ProjectID)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("%w: service account %q: project %q does not exist", ErrInvalid, sa.Meta.Name, sa.Spec.ProjectID)
	}
	if err := d.cfg.Stores.ServiceAccounts.Upsert(ctx, sa); err != nil {
		return err
	}
	d.notify(ctx, KindServiceAccount, OpUpsert, sa.Meta.ID)
	return nil
}
