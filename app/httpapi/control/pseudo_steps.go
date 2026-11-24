// The update and delete steps of a pseudo write: each runs the checks and writes the row's own update or delete route runs, through the same functions, and keeps the audit row that route records.

package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

func (w *pseudoWrite) write(ctx context.Context, tx *appcatalog.Stores, r pseudo.Row) error {
	switch r.Op {
	case pseudo.OpUpdate:
		return w.update(ctx, tx, r)
	case pseudo.OpDelete:
		return w.remove(ctx, tx, r)
	}
	return w.create(ctx, tx, r)
}

func (w *pseudoWrite) update(ctx context.Context, tx *appcatalog.Stores, r pseudo.Row) error {
	d := w.d.boundTo(tx)
	var fields []string
	var err error
	switch v := r.Value.(type) {
	case *policy.Policy:
		fields, err = updateStep(ctx, d.Stores.Policy, policyChecks(d), func(p *policy.Policy) *meta.Metadata { return &p.Meta }, v)
	case *pool.Pool:
		fields, err = updateStep(ctx, d.Stores.Pool, poolChecks(d), poolMeta, v)
	default:
		err = fmt.Errorf("no update for a %T row", r.Value)
	}
	if err != nil {
		return err
	}
	w.audit = append(w.audit, audit.Row{Action: authz.Plural(r.Kind) + ".update", Resource: d.auditResource(r.Kind, r.Meta.ID, r.Meta.Name, r.Meta.Owner), Fields: fields})
	return nil
}

func (w *pseudoWrite) remove(ctx context.Context, tx *appcatalog.Stores, r pseudo.Row) error {
	d := w.d.boundTo(tx)
	switch r.Value.(type) {
	case *pool.Pool:
		stored, err := storedRow(ctx, d.Stores.Pool, poolMeta, r)
		if err != nil {
			return err
		}
		dropped, err := checkDelete(ctx, poolChecks(d), d.refuseInUse, &stored.Meta, stored)
		if err != nil {
			return err
		}
		if err := d.Stores.Pool.Delete(ctx, stored.Meta.ID); err != nil {
			return err
		}
		w.audit = append(w.audit, audit.Row{Action: "pools.delete", Resource: d.auditResource("pool", stored.Meta.ID, stored.Meta.Name, stored.Meta.Owner), Fields: []string{audit.AnyField}})
		if dropped != nil {
			w.audit = append(w.audit, dropped.rows...)
		}
		return nil
	case *key.Key, *policy.Policy, *pseudo.Removal:
		return w.removeRow(ctx, d, r)
	}
	return fmt.Errorf("no delete for a %T row", r.Value)
}

func poolMeta(p *pool.Pool) *meta.Metadata { return &p.Meta }

// storedRow reads the stored row r's step changes, at the version r was read at: one written since answers meta.ErrStaleResourceVersion.
func storedRow[T any](ctx context.Context, store entityStore[T], metaOf func(*T) *meta.Metadata, r pseudo.Row) (*T, error) {
	v, err := store.Get(ctx, r.Meta.ID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", r.Kind, r.Meta.ID))
	}
	if r.Meta.ResourceVersion != "" && metaOf(v).ResourceVersion != r.Meta.ResourceVersion {
		return nil, meta.ErrStaleResourceVersion
	}
	pseudo.Record(ctx, r.Meta.ID, v)
	return v, nil
}

// updateStep runs the update checks on v, which replaces the stored row of its id, and writes it; it returns the fields the update changed. A stored row written since v was read is the store's meta.ErrStaleResourceVersion.
func updateStep[T any](ctx context.Context, store entityStore[T], checks kindChecks[T], metaOf func(*T) *meta.Metadata, v *T) ([]string, error) {
	m := metaOf(v)
	existing, err := store.Get(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", checks.singular, m.ID))
	}
	if err := checkUpdate(ctx, checks, metaOf, nil, m.ID, existing, v); err != nil {
		return nil, err
	}
	fields := audit.DiffFields(existing, v)
	return fields, updateRow(ctx, store, m, v)
}

// stepRows lists the rows the steps that do op write, in write order (pseudo.Changed).
func stepRows(rows []pseudo.Row, op pseudo.Op) []CreatedRow {
	out := []CreatedRow{}
	for _, r := range pseudo.Changed(rows, op) {
		out = append(out, CreatedRow{Kind: r.Kind, ID: r.Meta.ID, Name: r.Meta.Name})
	}
	return out
}
