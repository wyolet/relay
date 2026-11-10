// PUT /accounts/{ref}/policy sets the policy an account is bound to in one transaction: it takes the account out of the binding that holds it (deleting a binding left empty) and binds it alone in a new binding owned by the account's scope. Each write asks the grants and runs the guard its own policy-bindings call would. It never joins an existing binding: that binding's shared limits would start counting the account.

package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/audit"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/pkg/ids"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/slug"
)

type setAccountPolicyInput struct {
	Ref  string `path:"ref" doc:"Slug or id of the account."`
	Body struct {
		PolicyID *string `json:"policyId" doc:"Id of the policy to bind the account to; null unbinds it."`
	}
}

// AccountPolicy is the binding an account ends up in and the one it left.
type AccountPolicy struct {
	Binding                *policybinding.PolicyBinding `json:"binding" doc:"The binding that holds the account now; null when the account is unbound."`
	PreviousBindingID      string                       `json:"previousBindingId,omitempty" doc:"The binding the account was taken out of; omitted when nothing changed or the account was unbound before."`
	PreviousBindingDeleted bool                         `json:"previousBindingDeleted,omitempty" doc:"True when the previous binding held only this account and was deleted."`
}

type accountPolicyOutput struct {
	Body AccountPolicy
}

func registerAccountPolicy(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "set_account_policy",
		Method:      http.MethodPut,
		Path:        "/accounts/{ref}/policy",
		Summary:     "Set the policy an account is bound to",
		Description: "In one transaction, takes the account out of the policy binding that holds it, deleting that binding when it held only this account, and for a policyId binds the account alone in a new enabled binding owned by the account's owner. It never joins another binding of the same policy, whose shared limits would then count this account. When the holding binding already uses the policy and is enabled, nothing changes and it is answered. Each write needs what its own policy-bindings call needs: update (or delete) on the old binding, create in the account's scope, and the same reference checks, with the same answers. A holding binding the caller may not see answers 403. A concurrent change to the same rows answers 409 conflict; nothing was written.",
		Tags:        []string{"accounts"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
		Responses:   map[string]*huma.Response{"409": conflictResponse(api, "conflict: another change touched the account's binding at the same time, retry; duplicate_binding: another binding took the account first.")},
	}, func(ctx context.Context, in *setAccountPolicyInput) (*accountPolicyOutput, error) {
		return d.setAccountPolicy(ctx, in.Ref, in.Body.PolicyID)
	})
}

// policyChange is what setting an account's policy does to the binding holding it.
type policyChange struct {
	// unchanged answers the holding binding (or no binding) as it is.
	unchanged bool
	// shrunk is the holding binding without the account, nil when it is deleted or there is none.
	shrunk *policybinding.PolicyBinding
	// deleteHeld deletes the holding binding: the account was its only one.
	deleteHeld bool
	// create binds the account alone to the requested policy.
	create bool
}

// planPolicyChange decides the writes that move accountID from held (nil for none) to policyID (nil to unbind).
func planPolicyChange(held *policybinding.PolicyBinding, accountID string, policyID *string) policyChange {
	if held == nil {
		return policyChange{unchanged: policyID == nil, create: policyID != nil}
	}
	if policyID != nil && held.Spec.PolicyID == *policyID && held.IsEnabled() {
		return policyChange{unchanged: true}
	}
	c := policyChange{create: policyID != nil}
	left := slices.DeleteFunc(slices.Clone(held.Spec.AccountIDs), func(id string) bool { return id == accountID })
	if len(left) == 0 {
		c.deleteHeld = true
		return c
	}
	shrunk := *held
	shrunk.Spec.AccountIDs = left
	c.shrunk = &shrunk
	return c
}

func (d Deps) setAccountPolicy(ctx context.Context, ref string, policyID *string) (*accountPolicyOutput, error) {
	acc, err := d.findAccount(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out AccountPolicy
	var rows []audit.Row
	err = d.Stores.InTx(ctx, func(ctx context.Context, tx *appcatalog.Stores) error {
		var err error
		out, rows, err = d.boundTo(tx).applyAccountPolicy(ctx, acc, policyID)
		return err
	})
	if err != nil {
		if storage.IsConflict(err) || errors.Is(err, meta.ErrStaleResourceVersion) {
			return nil, &httpapi.APIError{
				Err: httpapi.APIErrorBody{
					Type:    "invalid_request_error",
					Code:    "conflict",
					Message: "another change touched this account's policy binding at the same time; nothing was changed, retry the request",
				},
				HTTPStatus: http.StatusConflict,
			}
		}
		var se huma.StatusError
		if errors.As(err, &se) {
			return nil, err
		}
		return nil, mapWriteErr("policy-binding", "", err)
	}
	if len(rows) > 0 {
		audit.RecordEach(ctx, rows)
	}
	return &accountPolicyOutput{Body: out}, nil
}

// applyAccountPolicy makes the writes planPolicyChange decides, each through its policy-bindings call's checks. d is bound to the transaction.
func (d Deps) applyAccountPolicy(ctx context.Context, acc *account.Account, policyID *string) (AccountPolicy, []audit.Row, error) {
	var out AccountPolicy
	held, err := d.Stores.PolicyBinding.Holding(ctx, acc.Meta.ID)
	if err != nil {
		return out, nil, huma.Error500InternalServerError(err.Error())
	}
	if held != nil && !visibleTo(ctx, d.Authz, "policy-binding", held.Meta.ID, held.Meta.Owner) {
		return out, nil, huma.Error403Forbidden(fmt.Sprintf("account %q is held by a policy binding you may not see", acc.Meta.Name))
	}
	c := planPolicyChange(held, acc.Meta.ID, policyID)
	if c.unchanged {
		out.Binding = held
		return out, nil, nil
	}
	var rows []audit.Row
	if held != nil {
		var row audit.Row
		if c.deleteHeld {
			row, err = d.deleteHeldBinding(ctx, held)
		} else {
			row, err = d.shrinkHeldBinding(ctx, held, c.shrunk)
		}
		if err != nil {
			return out, nil, err
		}
		rows = append(rows, row)
		out.PreviousBindingID, out.PreviousBindingDeleted = held.Meta.ID, c.deleteHeld
	}
	if c.create {
		created, row, err := d.createAccountBinding(ctx, acc, *policyID)
		if err != nil {
			return out, nil, err
		}
		rows = append(rows, row)
		out.Binding = created
	}
	return out, rows, nil
}

// shrinkHeldBinding saves next, held without the account, as PUT /policy-bindings/by-id/{id} would.
func (d Deps) shrinkHeldBinding(ctx context.Context, held, next *policybinding.PolicyBinding) (audit.Row, error) {
	checks := policyBindingChecks(d)
	if err := checks.mayUpdate(ctx, held.Meta.ID, &held.Meta); err != nil {
		return audit.Row{}, err
	}
	if err := checks.valid(next); err != nil {
		return audit.Row{}, err
	}
	if err := checks.guarded(ctx, "update", held, next); err != nil {
		return audit.Row{}, err
	}
	fields := audit.DiffFields(held, next)
	next.Meta.Dirty = true
	if err := d.Stores.PolicyBinding.Upsert(ctx, next); err != nil {
		return audit.Row{}, err
	}
	return d.bindingRow("policy-bindings.update", held, fields), nil
}

// deleteHeldBinding deletes held, which holds only the account, as DELETE /policy-bindings/by-id/{id} would.
func (d Deps) deleteHeldBinding(ctx context.Context, held *policybinding.PolicyBinding) (audit.Row, error) {
	checks := policyBindingChecks(d)
	if err := checks.mayDelete(ctx, held.Meta.ID, &held.Meta); err != nil {
		return audit.Row{}, err
	}
	if err := checks.guarded(ctx, "delete", held, nil); err != nil {
		return audit.Row{}, err
	}
	if _, err := d.refuseInUse(ctx, "policy-binding", held.Meta.ID, held.Meta.Name); err != nil {
		return audit.Row{}, err
	}
	if err := d.Stores.PolicyBinding.Delete(ctx, held.Meta.ID); err != nil {
		return audit.Row{}, err
	}
	return d.bindingRow("policy-bindings.delete", held, []string{audit.AnyField}), nil
}

// createAccountBinding binds the account alone to policyID in its owner's scope, as POST /policy-bindings would.
func (d Deps) createAccountBinding(ctx context.Context, acc *account.Account, policyID string) (*policybinding.PolicyBinding, audit.Row, error) {
	pbmeta := func(b *policybinding.PolicyBinding) *meta.Metadata { return &b.Meta }
	name := "policy"
	if p, err := d.Stores.Policy.Get(ctx, policyID); err == nil && p != nil {
		name = p.Meta.Name
	}
	b := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: ids.New(), Owner: acc.Meta.Owner},
		Spec: policybinding.Spec{PolicyID: policyID, AccountIDs: []string{acc.Meta.ID}},
	}
	b.Meta.Name = slug.Unique(slug.From(name+"-"+acc.Meta.Name), slugTakenFn(d.Stores.PolicyBinding, pbmeta))
	checks := policyBindingChecks(d)
	if err := checks.guarded(ctx, "create", nil, b); err != nil {
		return nil, audit.Row{}, err
	}
	if err := checks.mayCreate(ctx, &b.Meta.Owner); err != nil {
		return nil, audit.Row{}, err
	}
	if err := checks.valid(b); err != nil {
		return nil, audit.Row{}, err
	}
	if err := d.Stores.PolicyBinding.Upsert(ctx, b); err != nil {
		return nil, audit.Row{}, err
	}
	created, err := d.Stores.PolicyBinding.Get(ctx, b.Meta.ID)
	if err != nil || created == nil {
		return nil, audit.Row{}, fmt.Errorf("created policy binding %s but could not read it back: %v", b.Meta.ID, err)
	}
	return created, d.bindingRow("policy-bindings.create", created, []string{audit.AnyField}), nil
}

func (d Deps) bindingRow(action string, b *policybinding.PolicyBinding, fields []string) audit.Row {
	return audit.Row{Action: action, Resource: d.auditResource("policy-binding", b.Meta.ID, b.Meta.Name, b.Meta.Owner), Fields: fields}
}
