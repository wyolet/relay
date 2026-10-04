package apply

import (
	"context"
	"fmt"
	"sort"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/settings"
)

// kindWiring is the per-kind glue planKind needs: the documents, the name→id
// map they mint into, the stored rows, and the translate/store calls.
type kindWiring[D any, T any] struct {
	Kind   string
	Docs   []*D
	Names  map[string]string
	Rows   []*T
	To     func(D, manifest.Resolver) (*T, error)
	Meta   func(*T) *meta.Metadata
	Upsert func(context.Context, *T) error
	Delete func(context.Context, string) error
	// Check is a cross-entity rule the row's own Validate cannot see (it
	// reads one row, not the plan). Runs on create and update only, and
	// reports the same error the API's guard for that rule reports.
	Check func(context.Context, *T) error
	// Keep copies server-managed state from the stored row onto the declared
	// one before the diff, so neither the diff nor an update touches it.
	Keep func(prev, next *T)
}

func planKind[D any, T any](ctx context.Context, b *builder, k kindWiring[D, T]) error {
	route := KindRoutes[k.Kind]
	existing := make(map[string]*T, len(k.Rows))
	for _, row := range k.Rows {
		existing[k.Meta(row).Name] = row
	}

	declared := make(map[string]bool, len(k.Docs))
	for _, d := range k.Docs {
		wm := docMeta(d)
		name := wm.Name
		if declared[name] {
			return &DuplicateError{Kind: k.Kind, Name: name}
		}
		declared[name] = true

		obj, err := k.To(*d, b.idx)
		if err != nil {
			return fmt.Errorf("apply: %s %q: %w", k.Kind, name, err)
		}
		m := k.Meta(obj)
		m.ID = k.Names[name]
		// A declared row is owned by apply, so it is not hand-edited.
		m.Dirty = false
		// A user owner with no id names nobody; resolve it the way CRUD
		// create does. Cannot fail: only a supplied id is checked.
		if m.Owner.Kind == meta.OwnerUser && m.Owner.ID == "" {
			_ = refcheck.StampOwnerID(ctx, &m.Owner)
		}

		e := Entry{Kind: k.Kind, Name: name, ID: m.ID, plural: route.Plural, owner: m.Owner}
		if wm.ID != "" && wm.ID != m.ID {
			e.IDMismatch = wm.ID
		}

		prev, found := existing[name]
		if found {
			pm := k.Meta(prev)
			e.prev = rowState{present: true, dirty: pm.Dirty, updatedAt: pm.UpdatedAt}
		}
		switch {
		case !found:
			e.Action = ActionCreate
		case k.Meta(prev).Dirty && !b.opts.Force:
			e.Action = ActionSkipDirty
		default:
			// The stored owner wins: a document must not move a row into
			// another tenant's scope. An operator may still re-parent.
			if !b.admin {
				m.Owner = k.Meta(prev).Owner
			}
			e.owner = m.Owner
			if k.Keep != nil {
				k.Keep(prev, obj)
			}
			fields := changedFields(viewOf(k.Kind, prev, k.Meta(prev)), viewOf(k.Kind, obj, m))
			if len(fields) == 0 {
				e.Action = ActionUnchanged
			} else {
				e.Action = ActionUpdate
				e.ChangedFields = fields
			}
		}
		if e.Action == ActionCreate || e.Action == ActionUpdate {
			if err := validateRow(obj); err != nil {
				return &InvalidError{Kind: k.Kind, Name: name, Err: err}
			}
			if k.Check != nil {
				if err := k.Check(ctx, obj); err != nil {
					return &InvalidError{Kind: k.Kind, Name: name, Err: err}
				}
				// The check re-derives owners that mirror a spec field; a
				// document must not move a row into another scope that way.
				if found && !b.admin && m.Owner != k.Meta(prev).Owner {
					return &InvalidError{Kind: k.Kind, Name: name, Err: fmt.Errorf("owner is derived from the spec and cannot move from %s/%s", k.Meta(prev).Owner.Kind, k.Meta(prev).Owner.ID)}
				}
				e.owner = m.Owner
			}
			if b.declared == nil {
				b.declared = map[string]any{}
			}
			b.declared[m.ID] = obj
			if err := b.governs(settings.OpEdit, route.Singular, e.owner); err != nil {
				return &GovernanceError{Kind: k.Kind, Name: name, Err: err}
			}
			row := obj
			e.write = func(ctx context.Context) error { return k.Upsert(ctx, row) }
		}
		b.entries = append(b.entries, e)
	}

	var pruned []Entry
	if b.opts.Prune {
		for _, row := range k.Rows {
			m := k.Meta(row)
			if declared[m.Name] || !prunable(k.Kind, m.Name, m.Owner) || !b.selector.matches(m.Labels) {
				continue
			}
			if err := b.governs(settings.OpDelete, route.Singular, m.Owner); err != nil {
				return &GovernanceError{Kind: k.Kind, Name: m.Name, Err: err}
			}
			id := m.ID
			pruned = append(pruned, Entry{
				Kind: k.Kind, Name: m.Name, ID: id, Action: ActionDelete,
				plural: route.Plural, owner: m.Owner,
				prev:  rowState{present: true, dirty: m.Dirty, updatedAt: m.UpdatedAt},
				write: func(ctx context.Context) error { return k.Delete(ctx, id) },
			})
		}
		sort.Slice(pruned, func(i, j int) bool { return pruned[i].Name < pruned[j].Name })
	}
	b.deletes = append(b.deletes, pruned)
	return nil
}

// validateRow runs the kind's own Validate when it has one. Every catalog
// kind does; the assertion keeps planKind free of a per-kind wiring field.
func validateRow(row any) error {
	v, ok := row.(interface{ Validate() error })
	if !ok {
		return nil
	}
	return v.Validate()
}

// governs applies the governance:<kind> settings to a planned mutation. The
// system tier of settings.Governs is a rule about generic CRUD; apply is the
// declarative loader that owns system rows (providers, hosts, built-in
// roles), so system-owned rows are exempt here.
func (b *builder) governs(op settings.Op, kind string, owner meta.Owner) error {
	if b.opts.Gov == nil || owner.Kind == meta.OwnerSystem {
		return nil
	}
	return settings.Governs(b.opts.Gov, op, kind, string(owner.Kind), b.admin)
}
