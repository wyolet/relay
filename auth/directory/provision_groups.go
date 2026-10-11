package directory

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/auth/authz"
)

// ProvisionedGroup is what a source may set on a group.
type ProvisionedGroup struct {
	DisplayName string
	// ExternalID is the source's own id for the group; empty for none.
	ExternalID string
	// MemberIDs are user ids, each one of the source's accounts or already a member.
	MemberIDs []string
}

// Group returns one of the source's groups; a missing or foreign one is ErrNotFound.
func (p *Provisioner) Group(ctx context.Context, id string) (*Group, error) {
	return p.ownGroup(ctx, id)
}

// Groups returns every group the source manages, ordered by name.
func (p *Provisioner) Groups(ctx context.Context) ([]*Group, error) {
	all, err := p.d.cfg.Stores.Groups.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, g := range all {
		if p.owns(g.Spec.External) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (p *Provisioner) ownGroup(ctx context.Context, id string) (*Group, error) {
	g, err := p.d.cfg.Stores.Groups.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("%w: group %s", ErrNotFound, id)
	}
	if !p.owns(g.Spec.External) {
		return nil, foreign("group", id)
	}
	return g, nil
}

// CreateGroup adds a group the source manages. Its name derives from the display name and never changes after, and it is one no stored group holds and no binding names, so the new group inherits no grant an operator made for another.
func (p *Provisioner) CreateGroup(ctx context.Context, in ProvisionedGroup) (*Group, error) {
	name, err := p.freeGroupName(ctx, in.DisplayName)
	if err != nil {
		return nil, err
	}
	g := &Group{
		Meta: Metadata{ID: NewID(), Name: name, DisplayName: in.DisplayName, Owner: authz.Owner{Kind: OwnerSystem}},
		Spec: GroupSpec{MemberIDs: in.MemberIDs, External: External{Source: p.source, ID: in.ExternalID}},
	}
	if err := g.validate(); err != nil {
		return nil, err
	}
	if err := p.checkOwnMembers(ctx, in.MemberIDs, nil); err != nil {
		return nil, err
	}
	return p.writeGroup(ctx, g)
}

// writeGroup writes g and reads it back, so the caller holds the version the store assigned.
func (p *Provisioner) writeGroup(ctx context.Context, g *Group) (*Group, error) {
	if err := p.d.writeGroup(ctx, g); err != nil {
		return nil, err
	}
	stored, err := p.d.cfg.Stores.Groups.Get(ctx, g.Meta.ID)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("%w: group %s", ErrNotFound, g.Meta.ID)
	}
	return stored, nil
}

// UpdateGroup replaces the display name, external id and members of one of the source's groups, with no grant check on the members. A non-empty ifVersion refuses the write with ErrStaleResourceVersion unless the stored group still has it; either way the write is conditional on the version read here.
func (p *Provisioner) UpdateGroup(ctx context.Context, id string, in ProvisionedGroup, ifVersion string) (*Group, error) {
	g, err := p.ownGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	if ifVersion != "" && ifVersion != g.Meta.ResourceVersion {
		return nil, ErrStaleResourceVersion
	}
	if err := p.checkOwnMembers(ctx, in.MemberIDs, g.Spec.MemberIDs); err != nil {
		return nil, err
	}
	g.Meta.DisplayName = in.DisplayName
	g.Spec.External.ID = in.ExternalID
	g.Spec.MemberIDs = in.MemberIDs
	if err := g.validate(); err != nil {
		return nil, err
	}
	return p.writeGroup(ctx, g)
}

// DeleteGroup deletes one of the source's groups.
func (p *Provisioner) DeleteGroup(ctx context.Context, id string) error {
	if _, err := p.ownGroup(ctx, id); err != nil {
		return err
	}
	return deleteRow(ctx, p.d, p.d.cfg.Stores.Groups, KindGroup, id)
}

// checkOwnMembers refuses a member that is not one of the source's accounts, skipping the ones in known. A foreign account reads as missing, as it does everywhere else.
func (p *Provisioner) checkOwnMembers(ctx context.Context, ids, known []string) error {
	for _, id := range ids {
		if slices.Contains(known, id) {
			continue
		}
		u, err := p.d.cfg.Stores.Users.Get(ctx, id)
		if err != nil {
			return err
		}
		if u == nil || !p.owns(u.External) {
			return fmt.Errorf("%w: user %q does not exist", ErrInvalid, id)
		}
	}
	return nil
}

// freeGroupName slugs displayName into a name that no stored group holds and no binding names as a group subject.
func (p *Provisioner) freeGroupName(ctx context.Context, displayName string) (string, error) {
	base := Slugify(displayName)
	if base == "" {
		return "", fmt.Errorf("%w: group: a display name with a letter or digit is required", ErrInvalid)
	}
	groups, err := p.d.cfg.Stores.Groups.List(ctx)
	if err != nil {
		return "", err
	}
	bindings, err := p.d.cfg.Stores.RoleBindings.List(ctx)
	if err != nil {
		return "", err
	}
	taken := make(map[string]bool, len(groups))
	for _, g := range groups {
		taken[g.Meta.Name] = true
	}
	for _, b := range bindings {
		for _, s := range b.Spec.Subjects {
			if s.Kind == SubjectGroup && !strings.HasPrefix(s.Name, ReservedGroupPrefix) {
				taken[s.Name] = true
			}
		}
	}
	return UniqueName(base, func(n string) bool { return taken[n] }), nil
}
