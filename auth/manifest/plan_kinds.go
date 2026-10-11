package manifest

import (
	"context"
	"errors"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
)

type planner struct {
	reg  *Registry
	opts Options
	idx  *index
	rows *rows
	docs map[string][]*Document

	entries []Entry
	// deletes are collected per kind in write order and flushed in reverse, so children go before the parents they cascade from.
	deletes [][]Entry
}

type writeFunc[T any] func(context.Context, *directory.Directory, *principal.Principal, *T) error

// kindOps is the per-kind wiring planKind needs.
type kindOps[T any] struct {
	kind   string
	rows   []*T
	meta   func(*T) *directory.Metadata
	to     func(*Document, Resolver) (*T, error)
	from   func(*T, Namer) (*Document, error)
	create writeFunc[T]
	update writeFunc[T]
	remove func(*directory.Directory, context.Context, string) error
	// managed reports a row a provisioning source manages; nil for kinds none can.
	managed func(*T) bool
}

func (k kindOps[T]) isManaged(row *T) bool { return k.managed != nil && k.managed(row) }

// forced is dir writing over a managed row, which a planned write reaches only under Options.Force.
func forced(dir *directory.Directory, managed bool) *directory.Directory {
	if !managed {
		return dir
	}
	return dir.WithWriter(directory.Writer{Force: true})
}

// ungranted adapts a directory operation that hands out no permission and so takes no caller.
func ungranted[T any](op func(*directory.Directory, context.Context, *T) error) writeFunc[T] {
	return func(ctx context.Context, dir *directory.Directory, _ *principal.Principal, row *T) error {
		return op(dir, ctx, row)
	}
}

func granted[T any](op func(*directory.Directory, context.Context, *principal.Principal, *T) error) writeFunc[T] {
	return func(ctx context.Context, dir *directory.Directory, p *principal.Principal, row *T) error {
		return op(dir, ctx, p, row)
	}
}

func (p *planner) run() error {
	type dir = directory.Directory
	r, rs := p.reg, p.rows
	if err := planKind(p, kindOps[directory.Team]{kind: KindTeam, rows: rs.teams,
		meta: func(x *directory.Team) *directory.Metadata { return &x.Meta }, to: r.ToTeam, from: r.FromTeam,
		create: ungranted((*dir).CreateTeam), update: ungranted((*dir).UpdateTeam), remove: (*dir).DeleteTeam}); err != nil {
		return err
	}
	if err := planKind(p, kindOps[directory.Project]{kind: KindProject, rows: rs.projects,
		meta: func(x *directory.Project) *directory.Metadata { return &x.Meta }, to: r.ToProject, from: r.FromProject,
		create: ungranted((*dir).CreateProject), update: ungranted((*dir).UpdateProject), remove: (*dir).DeleteProject}); err != nil {
		return err
	}
	if err := planKind(p, kindOps[directory.ServiceAccount]{kind: KindServiceAccount, rows: rs.serviceAccounts,
		meta: func(x *directory.ServiceAccount) *directory.Metadata { return &x.Meta }, to: r.ToServiceAccount, from: r.FromServiceAccount,
		create: ungranted((*dir).CreateServiceAccount), update: ungranted((*dir).UpdateServiceAccount), remove: (*dir).DeleteServiceAccount}); err != nil {
		return err
	}
	if err := planKind(p, kindOps[directory.Role]{kind: KindRole, rows: rs.roles,
		meta: func(x *directory.Role) *directory.Metadata { return &x.Meta }, to: r.ToRole, from: r.FromRole,
		create: ungranted((*dir).CreateRole), update: granted((*dir).UpdateRole), remove: (*dir).DeleteRole}); err != nil {
		return err
	}
	if err := p.planUsers(); err != nil {
		return err
	}
	if err := planKind(p, kindOps[directory.Group]{kind: KindGroup, rows: rs.groups,
		meta: func(x *directory.Group) *directory.Metadata { return &x.Meta }, to: r.ToGroup, from: r.FromGroup,
		create: ungranted((*dir).CreateGroup), update: ungranted((*dir).UpdateGroup), remove: (*dir).DeleteGroup,
		managed: func(x *directory.Group) bool { return x.Spec.External.Source != "" }}); err != nil {
		return err
	}
	if err := planKind(p, kindOps[directory.RoleBinding]{kind: KindRoleBinding, rows: rs.bindings,
		meta: func(x *directory.RoleBinding) *directory.Metadata { return &x.Meta }, to: r.ToRoleBinding, from: r.FromRoleBinding,
		create: granted((*dir).CreateRoleBinding), update: granted((*dir).UpdateRoleBinding), remove: (*dir).DeleteRoleBinding}); err != nil {
		return err
	}
	for i := len(p.deletes) - 1; i >= 0; i-- {
		p.entries = append(p.entries, p.deletes[i]...)
	}
	return nil
}

func planKind[T any](p *planner, k kindOps[T]) error {
	stored := make(map[string]*T, len(k.rows))
	for _, row := range k.rows {
		stored[k.meta(row).Name] = row
	}
	declared := map[string]bool{}
	for _, d := range p.docs[k.kind] {
		name := d.Metadata.Name
		declared[name] = true
		if !directory.ValidName(name) {
			return docError(d, "metadata.name", errors.New("must be a DNS-1123 label of at most 63 characters"))
		}
		row, err := k.to(d, p.idx)
		if err != nil {
			return err
		}
		m := k.meta(row)
		m.ID, m.Dirty = p.idx.ids[k.kind][name], false
		e := Entry{Kind: k.kind, Name: name, ID: m.ID}
		if d.Metadata.ID != "" && d.Metadata.ID != m.ID {
			e.IDMismatch = d.Metadata.ID
		}
		prev, found := stored[name]
		switch {
		case !found:
			e.Action = ActionCreate
			e.write = func(ctx context.Context, dir *directory.Directory, c *principal.Principal) error {
				return k.create(ctx, dir, c, row)
			}
		case k.meta(prev).Dirty && !p.opts.Force:
			e.Action = ActionSkipDirty
		case k.isManaged(prev) && !p.opts.Force:
			e.Action = ActionSkipManaged
		default:
			// The update applies only while the row is as this plan read it.
			m.ResourceVersion = k.meta(prev).ResourceVersion
			before, err := k.from(prev, p.idx)
			if err != nil {
				return docError(d, "", err)
			}
			after, err := k.from(row, p.idx)
			if err != nil {
				return docError(d, "", err)
			}
			if e.ChangedFields = ChangedFields(before, after); len(e.ChangedFields) == 0 {
				e.Action = ActionUnchanged
				break
			}
			e.Action = ActionUpdate
			managed := k.isManaged(prev)
			e.write = func(ctx context.Context, dir *directory.Directory, c *principal.Principal) error {
				return k.update(ctx, forced(dir, managed), c, row)
			}
		}
		p.entries = append(p.entries, e)
	}
	var pruned []Entry
	if p.opts.Prune {
		for _, row := range k.rows {
			m := k.meta(row)
			if declared[m.Name] || !labelsMatch(p.opts.Selector, m.Labels) {
				continue
			}
			id, managed := m.ID, k.isManaged(row)
			if managed && !p.opts.Force {
				pruned = append(pruned, Entry{Kind: k.kind, Name: m.Name, ID: id, Action: ActionSkipManaged})
				continue
			}
			pruned = append(pruned, Entry{Kind: k.kind, Name: m.Name, ID: id, Action: ActionDelete,
				write: func(ctx context.Context, dir *directory.Directory, _ *principal.Principal) error {
					return k.remove(forced(dir, managed), ctx, id)
				}})
		}
	}
	p.deletes = append(p.deletes, pruned)
	return nil
}

// labelsMatch reports whether labels carry every pair in selector; an empty selector matches nothing.
func labelsMatch(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// planUsers plans accounts by username. Accounts have no dirty flag and no labels, so they are never skipped and never pruned.
func (p *planner) planUsers() error {
	stored := make(map[string]*directory.User, len(p.rows.users))
	for _, u := range p.rows.users {
		stored[u.Username] = u
	}
	for _, d := range p.docs[KindUser] {
		prev := stored[d.Metadata.Name]
		u, err := p.reg.ToUser(d, prev)
		if err != nil {
			return err
		}
		u.ID = p.idx.ids[KindUser][d.Metadata.Name]
		e := Entry{Kind: KindUser, Name: d.Metadata.Name, ID: u.ID}
		if d.Metadata.ID != "" && d.Metadata.ID != u.ID {
			e.IDMismatch = d.Metadata.ID
		}
		switch {
		case prev == nil:
			e.Action = ActionCreate
			e.write = func(ctx context.Context, dir *directory.Directory, c *principal.Principal) error {
				return dir.CreateUser(ctx, c, u)
			}
		default:
			if e.ChangedFields = ChangedFields(p.reg.FromUser(prev), p.reg.FromUser(u)); len(e.ChangedFields) == 0 {
				e.Action = ActionUnchanged
				break
			}
			e.Action = ActionUpdate
			e.write = func(ctx context.Context, dir *directory.Directory, c *principal.Principal) error {
				return dir.UpdateUser(ctx, c, u)
			}
		}
		p.entries = append(p.entries, e)
	}
	p.deletes = append(p.deletes, nil)
	return nil
}
