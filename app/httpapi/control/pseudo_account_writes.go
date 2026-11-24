// The account writes the pseudo kinds share: the checks POST /accounts runs, and an account's policy set through the code PUT /accounts/{ref}/policy runs.

package control

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/pseudo"
)

// providerAccountChecks are the checks POST /accounts runs.
func providerAccountChecks(d Deps) kindChecks[account.Account] {
	return kindChecks[account.Account]{
		plural: "accounts", singular: "account", authz: sharerAuthz{Authorizer: d.Authz, d: d}, gov: d.Catalog,
		validate: func(a *account.Account) error { return a.Validate() },
		guard:    guardAccount(d),
	}
}

// rebindAccount binds v's account through the code PUT /accounts/{ref}/policy runs, with its checks, and keeps its audit rows. The binding it makes becomes v's; the binding it took the account out of becomes v's Held as read before and Left as written, for the plan check.
func (w *pseudoWrite) rebindAccount(ctx context.Context, d Deps, v *pseudo.AccountRebinding) error {
	held, err := d.Stores.PolicyBinding.Holding(ctx, v.Account.Meta.ID)
	if err != nil {
		return err
	}
	if held != nil {
		pseudo.Record(ctx, held.Meta.ID, held)
	}
	var policyID *string
	if !v.Unbinds() {
		policyID = &v.PolicyID
	}
	out, rows, err := d.applyAccountPolicy(ctx, v.Account, policyID)
	if err != nil {
		return err
	}
	if len(rows) == 0 || (policyID != nil && out.Binding == nil) {
		return fmt.Errorf("account %q: setting its policy to %q wrote nothing", v.Account.Meta.Name, v.PolicyID)
	}
	if out.Binding != nil {
		v.Binding = *out.Binding
	}
	v.Held = held
	if held != nil && !out.PreviousBindingDeleted {
		if v.Left, err = d.Stores.PolicyBinding.Get(ctx, held.Meta.ID); err != nil {
			return err
		}
	}
	w.audit = append(w.audit, rows...)
	return nil
}
