package directory

import (
	"context"
	"fmt"
	"slices"
)

func groupMeta(g *Group) *Metadata { return &g.Meta }

// CreateGroup adds a group. An empty owner is the system.
func (d *Directory) CreateGroup(ctx context.Context, g *Group) error {
	s := d.cfg.Stores.Groups
	defaultOwner(&g.Meta)
	if err := prepareCreate(ctx, s, "group", &g.Meta, groupMeta); err != nil {
		return err
	}
	if err := g.validate(); err != nil {
		return err
	}
	if err := d.checkUsersExist(ctx, g.Spec.MemberIDs, nil); err != nil {
		return err
	}
	return d.writeGroup(ctx, g)
}

// UpdateGroup replaces the group with id g.Meta.ID, members included. A group a provisioning source manages is refused with ErrManaged unless the writer is that source or forced.
func (d *Directory) UpdateGroup(ctx context.Context, g *Group) error {
	stored, err := loadForUpdate(ctx, d.cfg.Stores.Groups, "group", &g.Meta, groupMeta)
	if err != nil {
		return err
	}
	if err := d.checkManaged(stored); err != nil {
		return err
	}
	if g.Spec.External.IsZero() {
		g.Spec.External = stored.Spec.External
	}
	defaultOwner(&g.Meta)
	if err := g.validate(); err != nil {
		return err
	}
	if err := d.checkUsersExist(ctx, g.Spec.MemberIDs, stored.Spec.MemberIDs); err != nil {
		return err
	}
	return d.writeGroup(ctx, g)
}

// DeleteGroup removes the group, refusing a managed one as UpdateGroup does. A binding naming it by name keeps the name, which may still match a group an identity provider asserts.
func (d *Directory) DeleteGroup(ctx context.Context, id string) error {
	g, err := d.cfg.Stores.Groups.Get(ctx, id)
	if err != nil {
		return err
	}
	if g != nil {
		if err := d.checkManaged(g); err != nil {
			return err
		}
	}
	return deleteRow(ctx, d, d.cfg.Stores.Groups, KindGroup, id)
}

// AddGroupMember adds the user to the group; adding a member twice does nothing. The write is conditional on the version read, so a concurrent membership change fails with ErrStaleResourceVersion instead of being lost.
func (d *Directory) AddGroupMember(ctx context.Context, groupID, userID string) error {
	return d.changeMembers(ctx, groupID, func(ids []string) ([]string, error) {
		if slices.Contains(ids, userID) {
			return nil, nil
		}
		if err := d.checkUsersExist(ctx, []string{userID}, nil); err != nil {
			return nil, err
		}
		return append(ids, userID), nil
	})
}

// RemoveGroupMember removes the user from the group; removing a non-member does nothing.
func (d *Directory) RemoveGroupMember(ctx context.Context, groupID, userID string) error {
	return d.changeMembers(ctx, groupID, func(ids []string) ([]string, error) {
		i := slices.Index(ids, userID)
		if i < 0 {
			return nil, nil
		}
		return slices.Delete(ids, i, i+1), nil
	})
}

// changeMembers applies edit to the stored member list; edit returns nil for no change.
func (d *Directory) changeMembers(ctx context.Context, groupID string, edit func([]string) ([]string, error)) error {
	g, err := d.cfg.Stores.Groups.Get(ctx, groupID)
	if err != nil {
		return err
	}
	if g == nil {
		return fmt.Errorf("%w: group %s", ErrNotFound, groupID)
	}
	if err := d.checkManaged(g); err != nil {
		return err
	}
	ids, err := edit(slices.Clone(g.Spec.MemberIDs))
	if err != nil || ids == nil {
		return err
	}
	g.Spec.MemberIDs = ids
	return d.writeGroup(ctx, g)
}

func (d *Directory) writeGroup(ctx context.Context, g *Group) error {
	if err := d.cfg.Stores.Groups.Upsert(ctx, g); err != nil {
		return err
	}
	d.notify(ctx, KindGroup, OpUpsert, g.Meta.ID)
	return nil
}

// checkUsersExist refuses an id in ids with no account, skipping the ones in known.
func (d *Directory) checkUsersExist(ctx context.Context, ids, known []string) error {
	for _, id := range ids {
		if slices.Contains(known, id) {
			continue
		}
		u, err := d.cfg.Stores.Users.Get(ctx, id)
		if err != nil {
			return err
		}
		if u == nil {
			return fmt.Errorf("%w: user %q does not exist", ErrInvalid, id)
		}
	}
	return nil
}
