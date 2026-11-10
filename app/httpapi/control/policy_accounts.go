// PUT /policies/{ref}/accounts sets the exact accounts a policy applies to in one transaction, so a form that picks several accounts never leaves some bound and some not. Each account moves through the same writes PUT /accounts/{ref}/policy makes.

package control

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
)

type setPolicyAccountsInput struct {
	Ref  string `path:"ref" doc:"Slug or id of the policy."`
	Body struct {
		AccountIDs []string `json:"accountIds" doc:"Ids of every account the policy should apply to. Accounts bound to the policy now and left out are unbound; an empty list unbinds them all."`
	}
}

// PolicyAccount is what setting a policy's accounts did to one account.
type PolicyAccount struct {
	AccountID              string                       `json:"accountId" doc:"The account."`
	Removed                bool                         `json:"removed,omitempty" doc:"True when the account was bound to the policy and left out of the list, so it is now unbound."`
	Binding                *policybinding.PolicyBinding `json:"binding" doc:"The binding that holds the account now; null for a removed account."`
	PreviousBindingID      string                       `json:"previousBindingId,omitempty" doc:"The binding the account was taken out of; omitted when nothing changed for it or it was unbound before."`
	PreviousBindingDeleted bool                         `json:"previousBindingDeleted,omitempty" doc:"True when the previous binding held only this account and was deleted."`
}

// PolicyAccounts lists the accounts a set-accounts call touched: every listed account, then every removed one.
type PolicyAccounts struct {
	Accounts []PolicyAccount `json:"accounts" doc:"Each listed account in request order, then each account the call unbound."`
}

type policyAccountsOutput struct {
	Body PolicyAccounts
}

func registerPolicyAccounts(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "set_policy_accounts",
		Method:      http.MethodPut,
		Path:        "/policies/{ref}/accounts",
		Summary:     "Set the accounts a policy applies to",
		Description: "In one transaction, makes accountIds exactly the accounts bound to the policy. A listed account already held by an enabled binding of the policy stays as it is; any other listed account moves as PUT /accounts/{ref}/policy moves it: out of the binding that holds it (deleting that binding when it held only this account, so an account under another policy changes policy) and alone into a new binding owned by its scope. An account bound to the policy and not listed is unbound. Each write needs what its own policy-bindings call needs, with the same answers; any refusal rolls the whole call back. Sending the same list again changes nothing. A listed account the caller may not see answers 404. A concurrent change to the same rows answers 409 conflict; nothing was written.",
		Tags:        []string{"policies"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
		Responses:   map[string]*huma.Response{"409": conflictResponse(api, "conflict: another change touched one of the bindings at the same time, retry; duplicate_binding: another binding took an account first.")},
	}, func(ctx context.Context, in *setPolicyAccountsInput) (*policyAccountsOutput, error) {
		return d.setPolicyAccounts(ctx, in.Ref, in.Body.AccountIDs)
	})
}

func (d Deps) setPolicyAccounts(ctx context.Context, ref string, accountIDs []string) (*policyAccountsOutput, error) {
	pol, err := d.findPolicy(ctx, ref)
	if err != nil {
		return nil, err
	}
	var listed []*account.Account
	for _, id := range accountIDs {
		if slices.ContainsFunc(listed, func(a *account.Account) bool { return a.Meta.ID == id }) {
			continue
		}
		acc, err := d.findAccount(ctx, id)
		if err != nil {
			return nil, err
		}
		listed = append(listed, acc)
	}
	var out PolicyAccounts
	var rows []audit.Row
	err = d.Stores.InTx(ctx, func(ctx context.Context, tx *appcatalog.Stores) error {
		var err error
		out, rows, err = d.boundTo(tx).applyPolicyAccounts(ctx, pol.Meta.ID, listed)
		return err
	})
	if err != nil {
		return nil, bindingTxErr(err, "this policy's bindings")
	}
	if len(rows) > 0 {
		audit.RecordEach(ctx, rows)
	}
	return &policyAccountsOutput{Body: out}, nil
}

// applyPolicyAccounts binds every listed account to policyID and unbinds the accounts bound to it that are not listed. d is bound to the transaction.
func (d Deps) applyPolicyAccounts(ctx context.Context, policyID string, listed []*account.Account) (PolicyAccounts, []audit.Row, error) {
	out := PolicyAccounts{Accounts: []PolicyAccount{}}
	var rows []audit.Row
	for _, acc := range listed {
		res, r, err := d.applyAccountPolicy(ctx, acc, &policyID)
		if err != nil {
			return out, nil, err
		}
		rows = append(rows, r...)
		out.Accounts = append(out.Accounts, PolicyAccount{AccountID: acc.Meta.ID, Binding: res.Binding,
			PreviousBindingID: res.PreviousBindingID, PreviousBindingDeleted: res.PreviousBindingDeleted})
	}
	stale, err := d.accountsBoundTo(ctx, policyID, listed)
	if err != nil {
		return out, nil, err
	}
	for _, acc := range stale {
		res, r, err := d.applyAccountPolicy(ctx, acc, nil)
		if err != nil {
			return out, nil, err
		}
		rows = append(rows, r...)
		out.Accounts = append(out.Accounts, PolicyAccount{AccountID: acc.Meta.ID, Removed: true,
			PreviousBindingID: res.PreviousBindingID, PreviousBindingDeleted: res.PreviousBindingDeleted})
	}
	return out, rows, nil
}

// accountsBoundTo returns the accounts bindings of policyID hold, other than the listed ones.
func (d Deps) accountsBoundTo(ctx context.Context, policyID string, listed []*account.Account) ([]*account.Account, error) {
	bindings, err := d.Stores.PolicyBinding.List(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	var out []*account.Account
	for _, b := range bindings {
		if b.Spec.PolicyID != policyID {
			continue
		}
		for _, id := range b.Spec.AccountIDs {
			if slices.ContainsFunc(listed, func(a *account.Account) bool { return a.Meta.ID == id }) {
				continue
			}
			acc, err := d.Stores.Account.Get(ctx, id)
			if err != nil {
				return nil, huma.Error500InternalServerError(err.Error())
			}
			if acc != nil {
				out = append(out, acc)
			}
		}
	}
	return out, nil
}

func (d Deps) findPolicy(ctx context.Context, ref string) (*policy.Policy, error) {
	notFound := huma.Error404NotFound(fmt.Sprintf("policy %q not found", ref))
	id := ref
	if !ids.Valid(id) {
		resolved, err := listScanResolver(d.Stores.Policy, func(p *policy.Policy) *meta.Metadata { return &p.Meta })(ref)
		if err != nil {
			return nil, notFound
		}
		id = resolved
	}
	p, err := d.Stores.Policy.Get(ctx, id)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if p == nil || !visibleTo(ctx, d.Authz, "policy", p.Meta.ID, p.Meta.Owner) {
		return nil, notFound
	}
	return p, nil
}
