// What expanding an agent removal reads, in the caller's view: the service account (404 when the caller may not see it), its account and the binding holding it, its keys and the shares it holds, every binding and policy (what still uses a labelled row, those the caller may not see included), and the policies and pools labelled for it with what the caller may do to each.

package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/meta"
)

func (d Deps) removalReads(ctx context.Context, body pseudo.AgentRemoval) (pseudo.RemovalReads, error) {
	var in pseudo.RemovalReads
	id := body.Spec.ServiceAccountID
	sa, err := d.Stores.ServiceAccount.Get(ctx, id)
	if err != nil {
		return in, err
	}
	if sa == nil || !visibleTo(ctx, d.Authz, "service-account", sa.Meta.ID, sa.Meta.Owner) {
		return in, refusal(huma.Error404NotFound(fmt.Sprintf("service account %q not found", id)), "/spec/serviceAccountId")
	}
	pseudo.Record(ctx, sa.Meta.ID, sa)
	in.ServiceAccount = sa
	if in.Lane, err = d.managedAccount(ctx, account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, sa.Meta.DisplayName, sa.Meta.Owner)); err != nil {
		return in, err
	}
	if in.Lane != nil {
		if in.Held, err = d.Stores.PolicyBinding.Holding(ctx, in.Lane.Meta.ID); err != nil {
			return in, err
		}
		if in.Held != nil {
			pseudo.Record(ctx, in.Held.Meta.ID, in.Held)
		}
	}
	keys, err := d.Stores.Key.List(ctx)
	if err != nil {
		return in, err
	}
	for _, k := range keys {
		if k.Spec.Principal == (key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}) {
			pseudo.Record(ctx, k.Meta.ID, k)
			in.Keys = append(in.Keys, k)
		}
	}
	accounts, err := d.Stores.Account.List(ctx)
	if err != nil {
		return in, err
	}
	for _, a := range accounts {
		if h := a.Spec.Holder; a.IsShared() && h != nil && h.Kind == account.HolderServiceAccount && h.ID == sa.Meta.ID {
			pseudo.Record(ctx, a.Meta.ID, a)
			in.Shares = append(in.Shares, a)
		}
	}
	if in.Bindings, err = d.Stores.PolicyBinding.List(ctx); err != nil {
		return in, err
	}
	if in.Policies, err = d.Stores.Policy.List(ctx); err != nil {
		return in, err
	}
	return in, d.readLabelled(ctx, sa.Meta.ID, sa.Meta.Owner, &in)
}

// readLabelled adds to in the policies, then the pools, labelled pseudo.AgentLabel=saID, each with whether the caller may see it, whether owner owns it, and whether its delete's grant and governance pass for the caller. The probes are not audited.
func (d Deps) readLabelled(ctx context.Context, saID string, owner meta.Owner, in *pseudo.RemovalReads) error {
	pd := d
	pd.Authz = unaudited(d.Authz)
	add := func(kind string, m *meta.Metadata, row any, record func(), mayDelete func() error) error {
		if m.Labels[pseudo.AgentLabel] != saID {
			return nil
		}
		l := pseudo.Labelled{Kind: kind, Meta: m, Value: row, Visible: visibleTo(ctx, d.Authz, kind, m.ID, m.Owner), InScope: m.Owner == owner}
		if !l.Visible && !l.InScope {
			in.Labelled = append(in.Labelled, l)
			return nil
		}
		record()
		if l.Visible && l.InScope {
			var err error
			if l.Deletable, err = probeAllowed(mayDelete()); err != nil {
				return err
			}
		}
		in.Labelled = append(in.Labelled, l)
		return nil
	}
	pc := policyChecks(pd)
	for _, p := range in.Policies {
		if err := add("policy", &p.Meta, p, func() { pseudo.Record(ctx, p.Meta.ID, p) }, func() error { return mayDeleteRow(ctx, pc, &p.Meta, p) }); err != nil {
			return err
		}
	}
	pools, err := d.Stores.Pool.List(ctx)
	if err != nil {
		return err
	}
	plc := poolChecks(pd)
	for _, p := range pools {
		if err := add("pool", &p.Meta, p, func() { pseudo.Record(ctx, p.Meta.ID, p) }, func() error { return mayDeleteRow(ctx, plc, &p.Meta, p) }); err != nil {
			return err
		}
	}
	return nil
}

// mayDeleteRow runs the grant, governance and guard of row's delete, as checkDelete does before its reference check.
func mayDeleteRow[T any](ctx context.Context, checks kindChecks[T], m *meta.Metadata, row *T) error {
	if err := checks.mayDelete(ctx, m.ID, m); err != nil {
		return err
	}
	return checks.guarded(ctx, "delete", row, nil)
}
