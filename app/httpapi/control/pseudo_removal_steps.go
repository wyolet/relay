// The delete steps of the agent removal kind: a relay key, a policy, a share revoked, and a service account with the account that goes with it. Each runs the checks and writes of its own delete route, through the same functions, and keeps the audit rows that route records.

package control

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/share"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// keyChecks are the checks the keys routes run.
func keyChecks(d Deps) kindChecks[key.Key] {
	return kindChecks[key.Key]{
		plural: "keys", singular: "key", authz: d.Authz, gov: d.Catalog,
		validate: func(k *key.Key) error { return k.Validate() },
		guard:    guardKeyAccount(d),
	}
}

func (w *pseudoWrite) removeRow(ctx context.Context, d Deps, r pseudo.Row) error {
	var rows []audit.Row
	var err error
	switch v := r.Value.(type) {
	case *key.Key:
		rows, err = deleteStep(ctx, d, d.Stores.Key, keyChecks(d), func(k *key.Key) *meta.Metadata { return &k.Meta }, r)
	case *policy.Policy:
		rows, err = deleteStep(ctx, d, d.Stores.Policy, policyChecks(d), func(p *policy.Policy) *meta.Metadata { return &p.Meta }, r)
	case *pseudo.Removal:
		switch row := v.Row.(type) {
		case *account.Account:
			rows, err = d.revokeShareStep(ctx, v, row, r)
		case *serviceaccount.ServiceAccount:
			rows, err = d.deleteServiceAccountStep(ctx, v, r)
		default:
			err = fmt.Errorf("no delete for a %T row", v.Row)
		}
	}
	if err != nil {
		return err
	}
	w.audit = append(w.audit, rows...)
	return nil
}

// deleteStep deletes r's row as DELETE /{plural}/by-id/{id} does, refusing while anything references it, and returns the audit rows that route records.
func deleteStep[T any](ctx context.Context, d Deps, store entityStore[T], checks kindChecks[T], metaOf func(*T) *meta.Metadata, r pseudo.Row) ([]audit.Row, error) {
	stored, err := storedRow(ctx, store, metaOf, r)
	if err != nil {
		return nil, err
	}
	m := metaOf(stored)
	dropped, err := checkDelete(ctx, checks, d.refuseInUse, m, stored)
	if err != nil {
		return nil, err
	}
	if err := store.Delete(ctx, m.ID); err != nil {
		return nil, err
	}
	return deletedRows(d, checks.plural, r.Kind, m, dropped), nil
}

// revokeShareStep revokes the share s as DELETE /accounts/by-id/{id} does, and fills rm.Along with what the revoke changed: each binding it deleted as read before, each binding, key and pool it changed as it left them.
func (d Deps) revokeShareStep(ctx context.Context, rm *pseudo.Removal, s *account.Account, r pseudo.Row) ([]audit.Row, error) {
	stored, err := storedRow(ctx, d.Stores.Account, func(a *account.Account) *meta.Metadata { return &a.Meta }, r)
	if err != nil {
		return nil, err
	}
	if !stored.IsShared() {
		return nil, fmt.Errorf("account %q is not a share", s.Meta.Name)
	}
	if _, err := checkDelete(ctx, providerAccountChecks(d), nil, &stored.Meta, stored); err != nil {
		return nil, err
	}
	before, err := d.Stores.PolicyBinding.List(ctx)
	if err != nil {
		return nil, err
	}
	held := map[string]*policybinding.PolicyBinding{}
	for _, b := range before {
		held[b.Meta.ID] = b
	}
	changes, err := share.Revoke(ctx, d.shareStores(), stored.Meta.ID)
	if err != nil {
		return nil, err
	}
	rm.Along = nil
	for _, c := range changes {
		row, err := d.changedByRevoke(ctx, c, held[c.ID])
		if err != nil {
			return nil, err
		}
		row.Pointer = r.Pointer
		rm.Along = append(rm.Along, row)
	}
	return deletedRows(d, "accounts", "account", &stored.Meta, d.revokedLinks(ctx, changes)), nil
}

// changedByRevoke is the row c names as the revoke left it: a deleted binding as it was before, wasHeld; anything else read again.
func (d Deps) changedByRevoke(ctx context.Context, c share.Change, wasHeld *policybinding.PolicyBinding) (pseudo.Row, error) {
	if c.Deleted {
		if wasHeld == nil {
			return pseudo.Row{}, fmt.Errorf("the revoke deleted policy binding %q, which was not read before it", c.Name)
		}
		pseudo.Record(ctx, wasHeld.Meta.ID, wasHeld)
		return pseudo.Row{Kind: c.Kind, Meta: &wasHeld.Meta, Value: wasHeld, Op: pseudo.OpDelete}, nil
	}
	var v any
	var m *meta.Metadata
	switch c.Kind {
	case "policy-binding":
		if wasHeld != nil {
			pseudo.Record(ctx, wasHeld.Meta.ID, wasHeld)
		}
		b, err := d.Stores.PolicyBinding.Get(ctx, c.ID)
		if err != nil || b == nil {
			return pseudo.Row{}, fmt.Errorf("policy binding %q after the revoke: %v", c.Name, err)
		}
		v, m = b, &b.Meta
	case "key":
		k, err := d.Stores.Key.Get(ctx, c.ID)
		if err != nil || k == nil {
			return pseudo.Row{}, fmt.Errorf("key %q after the revoke: %v", c.Name, err)
		}
		v, m = k, &k.Meta
	case "pool":
		p, err := d.Stores.Pool.Get(ctx, c.ID)
		if err != nil || p == nil {
			return pseudo.Row{}, fmt.Errorf("pool %q after the revoke: %v", c.Name, err)
		}
		v, m = p, &p.Meta
	default:
		return pseudo.Row{}, fmt.Errorf("the revoke changed a %s, which the plan check cannot read", c.Kind)
	}
	return pseudo.Row{Kind: c.Kind, Meta: m, Value: v, Op: pseudo.OpUpdate}, nil
}

// deleteServiceAccountStep deletes the service account as DELETE /service-accounts/by-id/{id} does, and checks that every row rm takes along went with it.
func (d Deps) deleteServiceAccountStep(ctx context.Context, rm *pseudo.Removal, r pseudo.Row) ([]audit.Row, error) {
	stored, err := storedRow(ctx, d.Stores.ServiceAccount, func(sa *serviceaccount.ServiceAccount) *meta.Metadata { return &sa.Meta }, r)
	if err != nil {
		return nil, err
	}
	dropped, err := checkDelete(ctx, serviceAccountChecks(d), d.refuseInUse, &stored.Meta, stored)
	if err != nil {
		return nil, err
	}
	if err := newDirectoryKinds(d, newDirectoryOps(d)).serviceAccounts.Delete(ctx, stored.Meta.ID); err != nil {
		if managed := managedError(err); managed != nil {
			return nil, managed
		}
		return nil, err
	}
	for _, a := range rm.Along {
		left, err := d.Stores.Account.Get(ctx, a.Meta.ID)
		if err != nil {
			return nil, err
		}
		if left != nil {
			return nil, fmt.Errorf("account %q did not go with service account %q", a.Meta.Name, stored.Meta.Name)
		}
	}
	return deletedRows(d, "service-accounts", "service-account", &stored.Meta, dropped), nil
}

// deletedRows are the audit rows a delete route records: the row deleted, then each row it took a link from.
func deletedRows(d Deps, plural, kind string, m *meta.Metadata, dropped *droppedLinks) []audit.Row {
	rows := []audit.Row{{Action: plural + ".delete", Resource: d.auditResource(kind, m.ID, m.Name, m.Owner), Fields: []string{audit.AnyField}}}
	if dropped != nil {
		rows = append(rows, dropped.rows...)
	}
	return rows
}
