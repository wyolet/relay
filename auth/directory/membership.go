package directory

import (
	"context"
	"fmt"
	"slices"

	"github.com/wyolet/relay/auth/principal"
)

// CheckMembershipGrants runs the grant check that writing g would run, against the stored group with g's id (none for a create), and writes nothing. A product planning several writes calls it to refuse a group before any write lands; the write checks again.
func (d *Directory) CheckMembershipGrants(ctx context.Context, p *principal.Principal, g *Group) error {
	var stored *Group
	if g.Meta.ID != "" {
		var err error
		if stored, err = d.cfg.Stores.Groups.Get(ctx, g.Meta.ID); err != nil {
			return err
		}
	}
	return d.checkAddedMembers(ctx, p, stored, g)
}

// checkAddedMembers runs the grant check for every role bound to the group, at each binding's scope, when the write gives a user the group: a member acts under every binding that names it, so adding one hands those roles out. A write that only removes members needs no check.
func (d *Directory) checkAddedMembers(ctx context.Context, p *principal.Principal, stored, g *Group) error {
	if !addsMembers(stored, g) {
		return nil
	}
	bindings, err := d.cfg.Stores.RoleBindings.List(ctx)
	if err != nil {
		return err
	}
	subject := Subject{Kind: SubjectGroup, Name: g.Meta.Name}
	roles := map[string]*Role{}
	for _, b := range bindings {
		if !slices.Contains(b.Spec.Subjects, subject) {
			continue
		}
		r, seen := roles[b.Spec.RoleID]
		if !seen {
			if r, err = d.cfg.Stores.Roles.Get(ctx, b.Spec.RoleID); err != nil {
				return err
			}
			roles[b.Spec.RoleID] = r
		}
		if r == nil {
			continue
		}
		if err := d.checkGrant(ctx, p, r, b.Spec.Scope); err != nil {
			return fmt.Errorf("group %q: adding members: %w", g.Meta.Name, err)
		}
	}
	return nil
}

// addsMembers reports whether writing g over stored may give any user the group's bindings. Bindings name a group by name, so a rename hands every member the bindings of the new name; re-enabling counts too, for a product that drops disabled groups from a principal's subjects.
func addsMembers(stored, g *Group) bool {
	if len(g.Spec.MemberIDs) == 0 {
		return false
	}
	if stored == nil || stored.Meta.Name != g.Meta.Name || (!stored.IsEnabled() && g.IsEnabled()) {
		return true
	}
	for _, id := range g.Spec.MemberIDs {
		if !slices.Contains(stored.Spec.MemberIDs, id) {
			return true
		}
	}
	return false
}
