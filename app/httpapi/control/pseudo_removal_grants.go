// What an agent removal asks of the caller before it answers any list: the visibility, grant and governance each delete step will ask in the transaction, the service account's first. A refused caller learns nothing of the rows the removal would touch.

package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

// mayRemove refuses rows a step of which the caller could not take: the service account's delete first, then each other step's in order, the binding the lane leaves as PUT /accounts/{ref}/policy takes it. A row the caller may not see answers 404 without naming it. Every refusal points at /spec/serviceAccountId.
func (d Deps) mayRemove(ctx context.Context, rows []pseudo.Row) error {
	for _, r := range rows {
		if rm, ok := r.Value.(*pseudo.Removal); ok {
			if sa, ok := rm.Row.(*serviceaccount.ServiceAccount); ok {
				if err := mayDeleteSeen(ctx, serviceAccountChecks(d), &sa.Meta, sa); err != nil {
					return err
				}
			}
		}
	}
	for _, r := range rows {
		var err error
		switch v := r.Value.(type) {
		case *key.Key:
			err = mayDeleteSeen(ctx, keyChecks(d), &v.Meta, v)
		case *policy.Policy:
			err = mayDeleteSeen(ctx, policyChecks(d), &v.Meta, v)
		case *pool.Pool:
			err = mayDeleteSeen(ctx, poolChecks(d), &v.Meta, v)
		case *pseudo.AccountRebinding:
			err = d.mayUnbind(ctx, v)
		case *pseudo.Removal:
			if a, ok := v.Row.(*account.Account); ok {
				err = mayDeleteSeen(ctx, providerAccountChecks(d), &a.Meta, a)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// mayDeleteSeen runs the visibility, grant, governance and guard of row's delete, refusing a row the caller may not see without naming it.
func mayDeleteSeen[T any](ctx context.Context, checks kindChecks[T], m *meta.Metadata, row *T) error {
	if !visibleTo(ctx, checks.authz, checks.singular, m.ID, m.Owner) {
		return hiddenStep()
	}
	if err := mayDeleteRow(ctx, checks, m, row); err != nil {
		return refusal(err, "/spec/serviceAccountId")
	}
	return nil
}

// mayUnbind asks what PUT /accounts/{ref}/policy with a null policyId asks of the binding the lane leaves: to see it, then its delete, or its update to the accounts it keeps.
func (d Deps) mayUnbind(ctx context.Context, rb *pseudo.AccountRebinding) error {
	held := rb.Held
	if held == nil {
		return nil
	}
	if !visibleTo(ctx, d.Authz, "policy-binding", held.Meta.ID, held.Meta.Owner) {
		return refusal(huma.Error403Forbidden(fmt.Sprintf("account %q is held by a policy binding you may not see", rb.Account.Meta.Name)), "/spec/serviceAccountId")
	}
	checks := policyBindingChecks(d)
	var err error
	if rb.Left == nil {
		if err = checks.mayDelete(ctx, held.Meta.ID, &held.Meta); err == nil {
			err = checks.guarded(ctx, "delete", held, nil)
		}
	} else if err = checks.mayUpdate(ctx, held.Meta.ID, &held.Meta); err == nil {
		err = checks.guarded(ctx, "update", held, rb.Left)
	}
	if err != nil {
		return refusal(err, "/spec/serviceAccountId")
	}
	return nil
}

func hiddenStep() error {
	return refusal(huma.Error404NotFound("the removal would delete a row you may not see; nothing was deleted"), "/spec/serviceAccountId")
}
