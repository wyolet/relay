// POST /pseudo-access sets what an existing team, project or service account may use in one request (app/pseudo.ExpandAccess, through the policy step).

package control

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/pseudo"
	"github.com/wyolet/relay/pkg/engine"
)

type pseudoAccessInput struct {
	DryRun bool `query:"dryRun" doc:"Run every check, the plan check included, and answer what would be written, without writing it. With spec.access omitted, answer only case and current."`
	Body   pseudo.Access
}

// PseudoAccessAnswer is what POST /pseudo-access wrote, or would write on a dry run.
type PseudoAccessAnswer struct {
	Case      pseudo.PolicyCase `json:"case"                enum:"none,own,copy" doc:"What the policy step did: none made a policy for an account in no binding, own changed the account's own policy in place, copy copied a policy shared or owned elsewhere into the target's scope and bound the account to the copy alone."`
	Current   *PolicyCurrent    `json:"current,omitempty"   doc:"With spec.access omitted on a dry run: the policy the account is under now, absent for none. Nothing else is answered then."`
	Unchanged bool              `json:"unchanged,omitempty" doc:"True when nothing needed writing: the account is in no binding and access is everyCredential without models, which is what it has already."`
	Policy    *RowRef           `json:"policy,omitempty"    doc:"The policy the account is under once written."`
	Pools     []RowRef          `json:"pools,omitempty"     doc:"The pools that policy links, in order."`
	Shares    []RowRef          `json:"shares,omitempty"    doc:"The shares made of accounts the target's scope did not reach."`
	Created   []CreatedRow      `json:"created,omitempty"   doc:"Every row made, in write order."`
	Updated   []CreatedRow      `json:"updated,omitempty"   doc:"Every row updated, in write order."`
	Deleted   []CreatedRow      `json:"deleted,omitempty"   doc:"Every row deleted, in write order."`
}

type pseudoAccessOutput struct {
	Status int
	Body   PseudoAccessAnswer
}

func registerPseudoAccess(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "create_pseudo_access",
		Method:      http.MethodPost,
		Path:        "/pseudo-access",
		Summary:     "Set what a team, project or service account may use",
		Description: "In one transaction, sets the whole access of the target's account (a team's or project's own account, a service account's account): access has the meaning and rules of POST /pseudo-agents' access. The policy the account is under is changed by case: none (the account is in no binding) makes a policy owned by the target's scope and binds the account alone; own (bound alone to a policy of the target's scope the caller may update) changes it in place; copy (a policy bound to other accounts too, a preset, or one owned elsewhere) copies it into the target's scope with its models, links, description and labels, changes the copy and binds the account to it alone, as PUT /accounts/{ref}/policy does. A policy or binding the caller may not see or change answers 403 policy_set_above naming the scope that holds it. " +
			"The policy's pools become poolIds, then one pool per host of accountIds; its models become access.models; its rate limits and budgets stay. The pools made are owned by the target's scope and labelled access-for=<account id>; on a later call the labelled pool of a host is updated in place when only the policy changed in place links it, and a labelled pool the policy no longer links that nothing else links is deleted. Other pools are only linked or unlinked. everyCredential links no pools; in case none without models nothing is written. A copy that would carry links the target's scope may not link answers 400 links_not_carried, unless access drops them. " +
			"Each row passes what its own create, update or delete call passes, with the same answers; then the result is checked against the plans (422 plan_issue) and against what every other account reaches (422 other_accounts_changed). dryRun=true answers 200 with the same body and writes nothing; with access omitted it answers only case and current.",
		Tags:          []string{"policies"},
		Middlewares:   protect,
		DefaultStatus: http.StatusCreated,
		Errors:        []int{400, 401, 403, 404, 500},
		Responses:     pseudoResponses(api, "conflict: another change touched the same rows at the same time, retry; share_exists or already_reachable from a share; duplicate_binding from the binding. pointer names the row."),
	}, func(ctx context.Context, in *pseudoAccessInput) (*pseudoAccessOutput, error) {
		out := &pseudoAccessOutput{Status: http.StatusOK}
		if in.Body.Spec.Access == nil && in.DryRun {
			c, cur, err := d.policyPrefill(ctx, in.Body.Spec.Target)
			out.Body = PseudoAccessAnswer{Case: c, Current: cur}
			return out, err
		}
		var live *engine.View
		if d.PlanView != nil {
			live = d.PlanView()
		}
		var change pseudo.AccessChange
		var targetID string
		poolNames := map[string]string{}
		ctx = pseudo.AfterCheck(ctx, func(after *engine.View) error {
			return checkPolicyStep(live, change.Rows, targetID)(after)
		})
		expand := func(ctx context.Context, tx *appcatalog.Stores) ([]pseudo.Row, error) {
			if err := pseudo.CheckAccess(in.Body); err != nil {
				return nil, err
			}
			reads, names, err := d.boundTo(tx).accessReads(ctx, in.Body)
			if err != nil {
				return nil, err
			}
			targetID = reads.Policy.Account.Meta.ID
			for id, name := range names {
				poolNames[id] = name
			}
			change, err = pseudo.ExpandAccess(in.Body, reads)
			return change.Rows, policyStepError(err)
		}
		_, rows, err := d.runPseudo(ctx, expand, in.DryRun)
		if err != nil {
			return nil, err
		}
		out.Body = accessAnswer(change, rows, poolNames)
		if !in.DryRun && len(rows) > 0 {
			out.Status = http.StatusCreated
		}
		return out, nil
	})
	documentPlanIssue(api, "/pseudo-access")
}

// accessAnswer answers what rows did, by role.
func accessAnswer(c pseudo.AccessChange, rows []pseudo.Row, poolNames map[string]string) PseudoAccessAnswer {
	out := PseudoAccessAnswer{Case: c.Case, Unchanged: c.Unchanged, Created: stepRows(rows, pseudo.OpCreate), Updated: stepRows(rows, pseudo.OpUpdate), Deleted: stepRows(rows, pseudo.OpDelete), Pools: []RowRef{}, Shares: []RowRef{}}
	for _, r := range rows {
		if a, ok := r.Value.(*account.Account); ok && r.Op == pseudo.OpCreate && a.IsShared() {
			out.Shares = append(out.Shares, rowRef(r.Meta))
		}
		if r.Kind == "pool" && r.Op != pseudo.OpDelete {
			poolNames[r.Meta.ID] = r.Meta.Name
		}
	}
	if p := c.Policy; p != nil {
		ref := rowRef(&p.Meta)
		out.Policy = &ref
		for _, id := range p.Spec.PoolIDs {
			out.Pools = append(out.Pools, RowRef{ID: id, Name: poolNames[id]})
		}
	}
	return out
}
